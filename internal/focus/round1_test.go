package focus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestAnInvisibleSubjectGetsNoRow: a subject the owner cannot see — however
// well-formed — is refused before anything is recorded, even with no key, so a
// caller cannot grow the table with arbitrary ids. A tail that turns out not to
// be readable (another conversation in the window) records nothing either.
func TestAnInvisibleSubjectGetsNoRow(t *testing.T) {
	ctx := context.Background()
	st := newMem()
	src := &fakeSource{tails: map[Subject][]Entry{}}
	r := &Refresher{Store: st, Sessions: src, Summarizer: &keyless{}}
	ghost := MirrorSubject("0b2c3d4e-ghost")
	if _, err := r.Refresh(ctx, 1, ghost); !errors.Is(err, ErrNoSession) {
		t.Fatalf("err = %v, want ErrNoSession", err)
	}
	if len(st.rows) != 0 || len(st.errs) != 0 {
		t.Fatalf("an invisible subject left rows %v / errors %v", st.rows, st.errs)
	}
	// Visible, but the tail read says it is not (it changed under us).
	moved := MirrorSubject("0b2c3d4e-moved")
	src.tails[moved] = nil
	sm := &fakeSummarizer{}
	r = &Refresher{Store: st, Sessions: &movingSource{fakeSource: src}, Summarizer: sm}
	if _, err := r.Refresh(ctx, 1, moved); !errors.Is(err, ErrNoSession) {
		t.Fatalf("moved: err = %v", err)
	}
	if len(st.errs) != 0 || sm.calls != 0 {
		t.Fatalf("a moved session recorded %v / called %d", st.errs, sm.calls)
	}
}

type movingSource struct{ *fakeSource }

func (m *movingSource) Tail(context.Context, Subject) ([]Entry, error) {
	return nil, fmt.Errorf("window holds another conversation: %w", ErrNoSession)
}

// TestToolLineNamesOnlyTheFile: a tool call reaches the summarizer as its name
// and, at most, the file it acts on — never a command, a body, a URL or any
// other argument.
func TestToolLineNamesOnlyTheFile(t *testing.T) {
	for _, c := range []struct{ tool, input, want string }{
		{"Bash", `{"command":"curl -H 'Authorization: Bearer FAKE-TOKEN-0002' https://x.invalid","description":"fetch"}`, "Bash"},
		{"Write", `{"file_path":"/repo/.env","content":"API_KEY=FAKE-TOKEN-0003"}`, "Write /repo/.env"},
		{"Edit", `{"file_path":"a.go","old_string":"x","new_string":"FAKE-TOKEN-0004"}`, "Edit a.go"},
		{"WebFetch", `{"url":"https://x.invalid/?token=FAKE-TOKEN-0005","prompt":"p"}`, "WebFetch"},
		{"Grep", `{"pattern":"FAKE-TOKEN-0006","path":"src"}`, "Grep src"},
		{"Custom", `{"token":"Bearer FAKE-TOKEN-0007"}`, "Custom"},
		{"Bash", `not json`, "Bash"},
	} {
		got := ToolLine(c.tool, c.input)
		if got != c.want || strings.Contains(got, "FAKE-TOKEN") {
			t.Errorf("ToolLine(%s, %s) = %q, want %q", c.tool, c.input, got, c.want)
		}
	}
}

// TestTheGlobalCapHolds: past GlobalPerHour calls across every owner, a new
// owner is capped too — though under its own per-owner cap.
func TestTheGlobalCapHolds(t *testing.T) {
	r, _, src, sm, _ := fixture()
	r.Now = func() time.Time { return time.Unix(3_000_000, 0) }
	ctx := context.Background()
	n := 0
	for owner := int64(1); n < GlobalPerHour; owner++ {
		for i := 0; i < PerOwnerPerHour && n < GlobalPerHour; i++ {
			s := AgentSubject(int64(10_000 + n))
			src.tails[s] = []Entry{{Role: "user", Text: fmt.Sprint(n)}}
			if _, err := r.Refresh(ctx, owner, s); err != nil {
				t.Fatalf("call %d: %v", n, err)
			}
			n++
		}
	}
	fresh := AgentSubject(99_999)
	src.tails[fresh] = []Entry{{Role: "user", Text: "x"}}
	if _, err := r.Refresh(ctx, 1_000, fresh); !errors.Is(err, ErrCapped) {
		t.Fatalf("a fresh owner past the global cap: err = %v, want ErrCapped", err)
	}
	if sm.calls != GlobalPerHour {
		t.Fatalf("%d calls, want %d", sm.calls, GlobalPerHour)
	}
}

// TestAPassRefreshesAtMostMaxPerPassPerOwner.
func TestAPassRefreshesAtMostMaxPerPassPerOwner(t *testing.T) {
	r, _, src, sm, _ := fixture()
	now := time.Now()
	for i := 0; i < MaxPerPass+5; i++ {
		s := AgentSubject(int64(2_000 + i))
		src.tails[s] = []Entry{{Role: "user", Text: fmt.Sprint(i)}}
		src.cands = append(src.cands, Candidate{Subject: s, ActiveAt: now.Add(-time.Duration(i) * time.Second)})
	}
	if _, err := r.RefreshOwner(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if sm.calls != MaxPerPass {
		t.Fatalf("%d refreshes in one pass, want %d", sm.calls, MaxPerPass)
	}
}

// TestARetentionPassMarksMirroredSessionsSeenAndPrunesTheRest: every visible
// mirrored candidate of every owner is touched, and rows unseen for Retention
// are pruned — with no summarizer and no refresh pass at all.
func TestARetentionPassMarksMirroredSessionsSeenAndPrunesTheRest(t *testing.T) {
	_, st, src, _, _ := fixture()
	st.owners = []int64{1}
	now := time.Unix(4_000_000, 0)
	r := &Retainer{Store: st, Sessions: src, AsOwner: func(ctx context.Context, _ int64) context.Context { return ctx },
		Retention: 72 * time.Hour, Now: func() time.Time { return now }}
	m := MirrorSubject("0b2c3d4e-live")
	src.cands = []Candidate{{Subject: m, ActiveAt: now}, {Subject: AgentSubject(5), ActiveAt: now}}
	if _, err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.touched) != 1 || st.touched[0] != m {
		t.Fatalf("touched %v, want only %v", st.touched, m)
	}
	if !st.prunedBefore.Equal(now.Add(-72 * time.Hour)) {
		t.Fatalf("pruned before %v, want %v", st.prunedBefore, now.Add(-72*time.Hour))
	}
}
