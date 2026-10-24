package focus

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// THE SERVER-SIDE REFRESH (operator decision: muster computes focus, the chief
// only reads it). A periodic pass (Run) comes here, and Refresh does one
// subject.
//
// One refresh of one subject:
//  1. an owner the summarizer cannot serve (no key) is refused before anything
//     is read;
//  2. the session's tail is read (Sessions.Tail) and rendered (RenderTail) — at
//     most MaxTailBytes — and fingerprinted;
//  3. 🔴 THE FINGERPRINT SKIP: when it equals the stored focus's fingerprint,
//     NOTHING is sent to the summarizer — the transcript has not moved since
//     the focus was computed, so neither would the focus. This is what keeps a
//     periodic pass over an idle fleet free;
//  4. the per-owner and global rate caps and the concurrency bound are taken;
//  5. the summarizer is called; its answer is parsed and Cleaned
//     (ParseSummary), and stored with the fingerprint — or, on any failure, the
//     error is RECORDED beside the focus the subject already had, which stays.

// KeyChecker is a Summarizer that can say, without being called, that it
// cannot serve owner (ScaleDown: the owner has no OpenRouter key).
type KeyChecker interface {
	CanSummarize(ctx context.Context, owner int64) error
}

// Candidate is a subject a pass may refresh, with when it was last active (zero
// = unknown, which counts as "may have moved").
type Candidate struct {
	Subject  Subject
	ActiveAt time.Time
}

// Sessions is where subjects and their transcripts come from (internal/api: the
// agents table and the mirror hub). ctx acts as one owner.
type Sessions interface {
	Candidates(ctx context.Context) ([]Candidate, error)
	// Visible is nil when s is a session of the owner's that can be read NOW,
	// and ErrNoSession otherwise. It reads no transcript.
	Visible(ctx context.Context, s Subject) error
	// Tail is s's conversation, newest last; ErrNoSession when it turned out
	// not to be readable (or to be another conversation) after all.
	Tail(ctx context.Context, s Subject) ([]Entry, error)
}

// The refresher's bounds.
const (
	// MaxConcurrent bounds summarizer calls in flight, across every owner.
	MaxConcurrent = 2
	// PerOwnerPerHour and GlobalPerHour cap summarizer calls in any hour.
	PerOwnerPerHour = 60
	GlobalPerHour   = 300
	// MaxPerPass bounds how many subjects one owner gets refreshed per pass.
	MaxPerPass = 20
	// DefaultInterval is the periodic pass (MUSTER_FOCUS_INTERVAL).
	DefaultInterval = 15 * time.Minute
	// DefaultRetention (MUSTER_FOCUS_RETENTION): a Claude session id is never
	// reused, so one unseen on every host for a week is a conversation that
	// ended; a week also outlasts a laptop left closed over a long weekend.
	DefaultRetention = 7 * 24 * time.Hour
)

// Outcome is what one refresh did.
type Outcome string

const (
	OutcomeUpdated   Outcome = "updated"
	OutcomeUnchanged Outcome = "unchanged" // the fingerprint skip: no summarizer call
	OutcomeEmpty     Outcome = "empty"     // no transcript to summarize
	OutcomeFailed    Outcome = "failed"    // recorded on the row; the old focus stays
)

// ErrCapped is a refresh the rate caps refused: nothing was called, and the
// refusal is recorded as rate-limited on the subject's row.
var ErrCapped = errors.New("focus: the summarizer rate cap is reached; try again later")

// Refresher runs refreshes.
type Refresher struct {
	Store      Store
	Sessions   Sessions
	Summarizer Summarizer
	// AsOwner is ctx acting as owner (internal/api's focusPrincipal).
	AsOwner func(ctx context.Context, owner int64) context.Context
	Logger  *log.Logger
	Now     func() time.Time
	sem     chan struct{}
	semOnce sync.Once
	mu      sync.Mutex
	calls   map[int64][]time.Time // per owner, the last hour's call times
	global  []time.Time
	// Calls counts summarizer calls made (a test reads it).
	Calls int
}

func (r *Refresher) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Refresher) logf(format string, args ...any) {
	if r.Logger != nil {
		r.Logger.Printf(format, args...)
	}
}

// take reserves one call for owner under the caps, or reports false.
func (r *Refresher) take(owner int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	cut := now.Add(-time.Hour)
	prune := func(ts []time.Time) []time.Time {
		i := sort.Search(len(ts), func(i int) bool { return ts[i].After(cut) })
		return ts[i:]
	}
	if r.calls == nil {
		r.calls = map[int64][]time.Time{}
	}
	r.global = prune(r.global)
	mine := prune(r.calls[owner])
	if len(mine) >= PerOwnerPerHour || len(r.global) >= GlobalPerHour {
		r.calls[owner] = mine
		return false
	}
	r.calls[owner] = append(mine, now)
	r.global = append(r.global, now)
	r.Calls++
	return true
}

func (r *Refresher) acquire(ctx context.Context) error {
	r.semOnce.Do(func() { r.sem = make(chan struct{}, MaxConcurrent) })
	select {
	case r.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Refresher) release() { <-r.sem }

// Refresh refreshes sub for owner, as owner (ctx must already act as owner).
// Whoever asks, an unchanged tail is the fingerprint skip: no summarizer call.
func (r *Refresher) Refresh(ctx context.Context, owner int64, sub Subject) (Outcome, error) {
	// 🔴 A SUBJECT NOBODY CAN SEE GETS NO ROW. Visibility is decided before
	// anything — even the key check — can record a failure, so a caller naming
	// arbitrary (well-formed) ids cannot grow agent_focus.
	if err := r.Sessions.Visible(ctx, sub); err != nil {
		return OutcomeFailed, err
	}
	cur, has, err := r.Store.Get(ctx, sub)
	if err != nil {
		return OutcomeFailed, err
	}
	// An owner the summarizer cannot serve (no OpenRouter key) costs no tail
	// read: a mirrored tail opens a transcript stream on the host.
	if k, ok := r.Summarizer.(KeyChecker); ok {
		if err := k.CanSummarize(ctx, owner); err != nil {
			r.record(ctx, sub, err)
			return OutcomeFailed, err
		}
	}
	entries, err := r.Sessions.Tail(ctx, sub)
	if err != nil {
		if !errors.Is(err, ErrNoSession) {
			r.record(ctx, sub, err)
		}
		return OutcomeFailed, err
	}
	tail, fp := RenderTail(entries)
	if tail == "" {
		return OutcomeEmpty, nil
	}
	if has && cur.Has() && cur.Fingerprint == fp {
		return OutcomeUnchanged, nil
	}
	if !r.take(owner) {
		r.record(ctx, sub, ErrCapped)
		return OutcomeFailed, ErrCapped
	}
	if err := r.acquire(ctx); err != nil {
		return OutcomeFailed, err
	}
	sum, err := r.Summarizer.Summarize(ctx, owner, tail)
	r.release()
	if err != nil {
		r.record(ctx, sub, err)
		return OutcomeFailed, err
	}
	if _, err := r.Store.Upsert(ctx, sub, Write{Title: sum.Title, Description: sum.Description, Source: SourceSummarizer,
		Fingerprint: fp}); err != nil {
		r.record(ctx, sub, err)
		return OutcomeFailed, err
	}
	return OutcomeUpdated, nil
}

// record stores cause's Reason — never its text, which can carry a provider's
// error body — and logs the full cause.
func (r *Refresher) record(ctx context.Context, sub Subject, cause error) {
	r.logf("focus: %s: refresh failed: %v", sub, cause)
	if err := r.Store.RecordError(context.WithoutCancel(ctx), sub, Reason(cause)); err != nil {
		r.logf("focus: %s: record the failure (%v): %v", sub, cause, err)
	}
}

// RefreshOwner refreshes owner's subjects that may have moved since their
// focus was written — active after it, or with none — at most MaxPerPass of
// them, most recently active first. ctx acts as owner.
func (r *Refresher) RefreshOwner(ctx context.Context, owner int64) (map[Outcome]int, error) {
	cands, err := r.Sessions.Candidates(ctx)
	if err != nil {
		return nil, err
	}
	have, err := r.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	updated := map[Subject]time.Time{}
	for _, f := range have {
		if f.Has() {
			updated[f.Subject] = f.UpdatedAt
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].ActiveAt.After(cands[j].ActiveAt) })
	out := map[Outcome]int{}
	n := 0
	for _, c := range cands {
		if n >= MaxPerPass || ctx.Err() != nil {
			break
		}
		if at, ok := updated[c.Subject]; ok && !c.ActiveAt.IsZero() && !c.ActiveAt.After(at) {
			continue
		}
		n++
		o, err := r.Refresh(ctx, owner, c.Subject)
		if errors.Is(err, ErrCapped) {
			out[OutcomeFailed]++
			break
		}
		out[o]++
	}
	return out, nil
}

// Pass refreshes every owner (Store.Owners), each as itself.
func (r *Refresher) Pass(ctx context.Context) error {
	if r.AsOwner == nil {
		return errors.New("focus: the refresher has no AsOwner")
	}
	owners, err := r.Store.Owners(ctx)
	if err != nil {
		return err
	}
	for _, o := range owners {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res, err := r.RefreshOwner(r.AsOwner(ctx, o), o)
		if err != nil {
			r.logf("focus: owner %d: %v", o, err)
			continue
		}
		if res[OutcomeUpdated]+res[OutcomeFailed] > 0 {
			r.logf("focus: owner %d: %s", o, fmt.Sprint(res))
		}
	}
	return nil
}

// Run passes every interval until ctx ends (interval <= 0: DefaultInterval).
func (r *Refresher) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := r.Pass(ctx); err != nil && ctx.Err() == nil {
			r.logf("focus: pass: %v", err)
		}
	}
}
