package focus

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// memStore is Store in memory, for one owner.
type memStore struct {
	rows         map[Subject]Focus
	errs         map[Subject]string
	owners       []int64
	touched      []Subject
	prunedBefore time.Time
}

func newMem() *memStore { return &memStore{rows: map[Subject]Focus{}, errs: map[Subject]string{}} }

func (m *memStore) Get(_ context.Context, s Subject) (Focus, bool, error) {
	f, ok := m.rows[s]
	return f, ok, nil
}
func (m *memStore) List(context.Context) ([]Focus, error) {
	var out []Focus
	for _, f := range m.rows {
		out = append(out, f)
	}
	return out, nil
}
func (m *memStore) Search(context.Context, string, int) ([]Hit, error) { return nil, nil }
func (m *memStore) Upsert(_ context.Context, s Subject, w Write) (Focus, error) {
	t, d, err := Clean(w.Title, w.Description)
	if err != nil {
		return Focus{}, err
	}
	f := Focus{Subject: s, Title: t, Description: d, Source: w.Source, Fingerprint: w.Fingerprint, UpdatedAt: time.Now()}
	m.rows[s] = f
	delete(m.errs, s)
	return f, nil
}
func (m *memStore) RecordError(_ context.Context, s Subject, msg string) error {
	m.errs[s] = msg
	return nil
}
func (m *memStore) Owners(context.Context) ([]int64, error) { return m.owners, nil }
func (m *memStore) Touch(_ context.Context, subs []Subject) error {
	m.touched = append(m.touched, subs...)
	return nil
}
func (m *memStore) Prune(_ context.Context, before time.Time) (int, error) {
	m.prunedBefore = before
	return 0, nil
}

type fakeSource struct {
	cands []Candidate
	tails map[Subject][]Entry
}

func (f *fakeSource) Candidates(context.Context) ([]Candidate, error) { return f.cands, nil }

// Visible: a subject is visible when the source has a tail for it (or lists it).
func (f *fakeSource) Visible(_ context.Context, s Subject) error {
	if _, ok := f.tails[s]; ok {
		return nil
	}
	return ErrNoSession
}
func (f *fakeSource) Tail(_ context.Context, s Subject) ([]Entry, error) {
	return f.tails[s], nil
}

type fakeSummarizer struct {
	calls int
	err   error
	sum   Summary
}

func (f *fakeSummarizer) Summarize(context.Context, int64, string) (Summary, error) {
	f.calls++
	return f.sum, f.err
}

func fixture() (*Refresher, *memStore, *fakeSource, *fakeSummarizer, Subject) {
	sub := AgentSubject(5)
	st, src := newMem(), &fakeSource{tails: map[Subject][]Entry{sub: {{Role: "user", Text: "fix the build"}, {Role: "assistant", Text: "patching"}}}}
	sm := &fakeSummarizer{sum: Summary{Title: "Fixing the build", Description: "Patching the Makefile."}}
	return &Refresher{Store: st, Sessions: src, Summarizer: sm}, st, src, sm, sub
}

// TestTheFingerprintSkipCallsNoSummarizer: a second refresh over an UNCHANGED
// tail makes no summarizer call; a tail that moved makes exactly one more.
func TestTheFingerprintSkipCallsNoSummarizer(t *testing.T) {
	r, st, src, sm, sub := fixture()
	ctx := context.Background()
	if o, err := r.Refresh(ctx, 1, sub); err != nil || o != OutcomeUpdated {
		t.Fatalf("first refresh: %v %v", o, err)
	}
	if sm.calls != 1 || st.rows[sub].Title != "Fixing the build" {
		t.Fatalf("positive control: calls=%d row=%+v", sm.calls, st.rows[sub])
	}
	o, err := r.Refresh(ctx, 1, sub)
	if err != nil || o != OutcomeUnchanged {
		t.Fatalf("unchanged refresh: %v %v, want %s", o, err, OutcomeUnchanged)
	}
	if sm.calls != 1 {
		t.Fatalf("the fingerprint skip did not hold: %d summarizer calls, want 1", sm.calls)
	}
	src.tails[sub] = append(src.tails[sub], Entry{Role: "assistant", Text: "tests green"})
	if o, _ := r.Refresh(ctx, 1, sub); o != OutcomeUpdated || sm.calls != 2 {
		t.Fatalf("moved tail: outcome %s, calls %d; want updated, 2", o, sm.calls)
	}
}

// TestAFailedRefreshKeepsTheFocusAndRecordsTheError.
func TestAFailedRefreshKeepsTheFocusAndRecordsTheError(t *testing.T) {
	r, st, src, sm, sub := fixture()
	ctx := context.Background()
	if _, err := r.Refresh(ctx, 1, sub); err != nil {
		t.Fatal(err)
	}
	src.tails[sub] = append(src.tails[sub], Entry{Role: "user", Text: "more"})
	sm.err = ErrRateLimited
	if o, err := r.Refresh(ctx, 1, sub); o != OutcomeFailed || !errors.Is(err, ErrRateLimited) {
		t.Fatalf("got %v %v", o, err)
	}
	if st.rows[sub].Title != "Fixing the build" {
		t.Errorf("the previous focus was not kept: %+v", st.rows[sub])
	}
	if st.errs[sub] != ReasonRateLimited {
		t.Errorf("the failure was not recorded as its reason: %q", st.errs[sub])
	}
}

// TestTheRateCapsHold: past PerOwnerPerHour calls an owner is refused with
// ErrCapped (and nothing is called); another owner is not; an hour later the
// first owner may call again.
func TestTheRateCapsHold(t *testing.T) {
	r, _, src, sm, _ := fixture()
	now := time.Unix(1_000_000, 0)
	r.Now = func() time.Time { return now }
	ctx := context.Background()
	for i := 0; i < PerOwnerPerHour; i++ {
		s := AgentSubject(int64(100 + i))
		src.tails[s] = []Entry{{Role: "user", Text: "x"}}
		if _, err := r.Refresh(ctx, 1, s); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	over := AgentSubject(999)
	src.tails[over] = []Entry{{Role: "user", Text: "y"}}
	if _, err := r.Refresh(ctx, 1, over); !errors.Is(err, ErrCapped) {
		t.Fatalf("past the per-owner cap: err = %v, want ErrCapped", err)
	}
	if sm.calls != PerOwnerPerHour {
		t.Fatalf("%d calls, want %d", sm.calls, PerOwnerPerHour)
	}
	if _, err := r.Refresh(ctx, 2, over); err != nil {
		t.Fatalf("another owner was capped too: %v", err)
	}
	now = now.Add(time.Hour + time.Second)
	src.tails[AgentSubject(997)] = []Entry{{Role: "user", Text: "z"}}
	if _, err := r.Refresh(ctx, 1, AgentSubject(997)); err != nil {
		t.Fatalf("an hour later: %v", err)
	}
}

// TestRefreshOwnerSkipsSubjectsThatHaveNotMoved: a pass refreshes a subject
// with no focus and one active after its focus, and leaves one whose focus is
// newer than its activity alone (no tail read, no call).
func TestRefreshOwnerSkipsSubjectsThatHaveNotMoved(t *testing.T) {
	r, st, src, sm, _ := fixture()
	now := time.Now()
	a, b, c := AgentSubject(1), AgentSubject(2), MirrorSubject("sess-c")
	st.rows[b] = Focus{Subject: b, Title: "old", Source: SourceSummarizer, UpdatedAt: now.Add(-time.Hour), Fingerprint: "x"}
	st.rows[c] = Focus{Subject: c, Title: "fresh", Source: SourceSummarizer, UpdatedAt: now, Fingerprint: "y"}
	src.cands = []Candidate{{a, now}, {b, now.Add(-time.Minute)}, {c, now.Add(-time.Hour)}}
	for _, s := range []Subject{a, b, c} {
		src.tails[s] = []Entry{{Role: "user", Text: s.String()}}
	}
	res, err := r.RefreshOwner(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if res[OutcomeUpdated] != 2 || sm.calls != 2 || st.rows[c].Title != "fresh" {
		t.Fatalf("res=%v calls=%d c=%+v", res, sm.calls, st.rows[c])
	}
}

func TestRenderTailBoundsAndFingerprint(t *testing.T) {
	var entries []Entry
	for i := 0; i < 500; i++ {
		entries = append(entries, Entry{Role: "assistant", Text: strings.Repeat("x", 3000)})
	}
	tail, fp := RenderTail(entries)
	if len(tail) > MaxTailBytes || len(strings.Split(tail, "\n")) > MaxTailEntries {
		t.Fatalf("tail is %d bytes, %d lines", len(tail), len(strings.Split(tail, "\n")))
	}
	if fp != Fingerprint(tail) || len(fp) != 64 {
		t.Fatalf("fingerprint %q", fp)
	}
	t1, f1 := RenderTail([]Entry{{"user", "a"}, {"assistant", "b"}})
	_, f2 := RenderTail([]Entry{{"user", "a"}, {"assistant", "c"}})
	if t1 != "user: a\nassistant: b" || f1 == f2 {
		t.Fatalf("t1=%q f1=%s f2=%s", t1, f1, f2)
	}
	if tail, fp := RenderTail(nil); tail != "" || fp != "" {
		t.Fatal("an empty tail has a fingerprint")
	}
}

// keyless is a summarizer that can serve nobody, and a source that counts
// tail reads.
type keyless struct{ fakeSummarizer }

func (keyless) CanSummarize(context.Context, int64) error { return ErrNoKey }

type countingSource struct {
	fakeSource
	reads int
}

func (c *countingSource) Tail(ctx context.Context, s Subject) ([]Entry, error) {
	c.reads++
	return c.fakeSource.Tail(ctx, s)
}

// TestAnOwnerWithNoKeyCostsNoTailRead: the refresh records ErrNoKey and reads
// no transcript (a mirrored tail opens a stream on the host) and calls nothing.
func TestAnOwnerWithNoKeyCostsNoTailRead(t *testing.T) {
	sub := AgentSubject(5)
	st := newMem()
	src := &countingSource{fakeSource: fakeSource{tails: map[Subject][]Entry{sub: {{Role: "user", Text: "x"}}}}}
	sm := &keyless{}
	r := &Refresher{Store: st, Sessions: src, Summarizer: sm}
	if o, err := r.Refresh(context.Background(), 1, sub); o != OutcomeFailed || !errors.Is(err, ErrNoKey) {
		t.Fatalf("got %s %v", o, err)
	}
	if src.reads != 0 || sm.calls != 0 || st.errs[sub] == "" {
		t.Fatalf("reads=%d calls=%d recorded=%q", src.reads, sm.calls, st.errs[sub])
	}
}
