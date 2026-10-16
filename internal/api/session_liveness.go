package api

import (
	"context"

	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// THE ONE PLACE SessionLink.DetailAvailable IS FILLED IN.
//
// internal/notes used to compute it in SQL, off a join against the session
// table. It may not any more, and the reason is the entire premise of this
// repository: the domain layer moved here and the session table did not. That
// join was the one cross-service read in the whole carve, and deleting it is
// what made the domain layer movable at all.
//
// 🔴 THE COMPOSITION LIVES AT THE API LAYER BECAUSE OF DEPENDENCY DIRECTION,
// NOT CONVENIENCE. Putting it back inside internal/notes would mint exactly the
// dependency the carve removed — a domain package reaching for a store that
// belongs to another service — and it would do so invisibly, since the call
// would compile and the tests would pass against a local fake. Here, the
// dependency is a PORT (SessionLivenessProbe) that the binary fills with an
// HTTP client, so the network hop is visible in the wiring rather than buried
// in a query.
//
// 🔴 IT IS A DECORATOR AROUND THE STORE, NOT A HELPER HANDLERS CALL, AND THE
// DIFFERENCE IS THE WHOLE POINT. `s.ext.Notes.Get` has twenty-odd call sites in
// this package and `.Sessions` is embedded on every Note they return, so a helper
// would be a thing to forget at each of them — and forgetting is SILENT and
// CONFIDENT: DetailAvailable defaults to false, false renders the words "no
// transcript recorded" (or "transcript expired") over a session whose transcript is
// perfectly alive, and nothing errors, logs or 404s. That is the same failure shape
// as the task-create directory picker, which was empty in production from the day
// it shipped because its query SUCCEEDED and returned nothing. Wrapping the store
// at the single wiring point (UseExtensions) means a handler cannot opt out; it is
// the same argument notes.WithStatusLogging is built on.
//
// 🔴 A FAILED LIVENESS READ IS AN ERROR, NOT A false. Before this change the probe
// was a join inside the same statement, so "the session table could not be read"
// and "the task could not be read" were ONE outcome: the read failed and the caller
// saw it. Degrading to false here would convert a transient database fault into the
// permanent-looking sentence "no transcript recorded" on every row of the page —
// the seam route refuses exactly that (it answers 500, not 404, for the same
// reason). So the error propagates and the read fails, as it did before.
// ---------------------------------------------------------------------------

// SessionLivenessProbe is the batched existence read this package needs from
// whatever holds the session records. Declared consumer-side and NARROW: the
// decorator wants one bit per id and must not be able to reach for a transcript
// tail, a project name or anything else on the row.
//
// 🔴 IT IS BATCHED EVEN THOUGH THE ONLY IMPLEMENTATION IS PER-ID, AND THAT IS
// DELIBERATE. internal/router satisfies this by issuing one request per id
// behind the batched signature. Keeping the batch shape at the port means the
// decorator resolves a whole board read in ONE call — the N+1 the extraction
// plan explicitly refused to build — and it means a batch form of the upstream
// route, if one ever lands, drops in here without touching a single caller. A
// per-id port would have pushed the fan-out into the decorator and made that
// substitution a rewrite.
type SessionLivenessProbe interface {
	// SessionsExisting reports which of sessionIDs still have a transcript record.
	// Ids absent from the map have none.
	SessionsExisting(ctx context.Context, sessionIDs []string) (map[string]bool, error)
}

// liveNotes is a notes.Store that fills SessionLink.DetailAvailable on everything
// it hands back.
//
// 🔴 IT OVERRIDES EVERY notes.Store METHOD WHOSE RESULT CAN CARRY A SessionLink,
// not just the two that carry one today. TestLiveNotesOverridesEveryLinkBearingRead
// derives that set by reflection over the interface and fails when it grows — a
// method added to notes.Store that returns a Note would otherwise be promoted
// straight off the embedded interface, uncomposed and silent.
type liveNotes struct {
	notes.Store
	probe SessionLivenessProbe
}

// withSessionLiveness wraps st so every link it returns carries a composed
// DetailAvailable.
//
// 🔴 A nil STORE STAYS A nil INTERFACE. Half this package branches on
// `s.ext.Notes == nil` to decide whether a route exists at all; returning a
// non-nil wrapper around a nil store would make that check false and nil-panic
// later, which internal/notes' own statuslog tests already record as a trap.
//
// 🔴 A nil PROBE IS TOLERATED HERE AND REFUSED AT THE DOOR, AND THE SPLIT IS
// THE ANSWER TO "MAKE FORGETTING-TO-COMPOSE HARD TO DO".
//
// Tolerated here, because this package's own fixtures wire a notes store with no
// probe in a great many places and making that a panic would be a large,
// unrelated churn — and because a panic in a library constructor is a worse
// failure mode than the one it prevents.
//
// Refused at the door, because a running server in that state renders "no
// transcript recorded" over live transcripts on every surface, silently. So
// UseExtensions records it as a READINESS DEFECT: a Server with Notes set and no
// SessionLiveness probe never reports ready, /readyz answers 503 naming the
// missing field, and an orchestrator will not put the pod into service. The
// failure moves from "wrong sentence on a page nobody double-checks" to "the
// deployment does not go ready", which is the loudest cheap signal available.
// See Extensions.defects and handleReady.
//
// The two guards that hold this down are TestLiveNotesOverridesEveryLinkBearingRead
// (every link-bearing read IS composed, derived by reflection so the set cannot
// silently grow) and TestUncomposedNotesStoreIsNotReady (the door refuses).
func withSessionLiveness(st notes.Store, probe SessionLivenessProbe) notes.Store {
	if st == nil {
		return nil
	}
	return &liveNotes{Store: st, probe: probe}
}

// resolve performs ONE probe for every link in every batch and writes the answer
// onto each link in place.
//
// Batches are passed by pointer-to-slice-element (the callers hand it the slices
// they are about to return), so nothing is copied back.
func (l *liveNotes) resolve(ctx context.Context, batches ...[]notes.SessionLink) error {
	if l.probe == nil {
		return nil
	}
	n := 0
	for _, b := range batches {
		n += len(b)
	}
	if n == 0 {
		// No links, no query. The board read hits this on every page that happens to
		// hold no threads, and a store that queried anyway would pay a round trip for
		// an empty IN list.
		return nil
	}
	ids := make([]string, 0, n)
	for _, b := range batches {
		for i := range b {
			ids = append(ids, b[i].SessionID)
		}
	}
	live, err := l.probe.SessionsExisting(ctx, ids)
	if err != nil {
		return err
	}
	for _, b := range batches {
		for i := range b {
			b[i].DetailAvailable = live[b[i].SessionID]
		}
	}
	return nil
}

// resolveNotes composes liveness across a slice of notes' embedded threads in ONE
// probe — the board read resolves every card's thread together rather than once per
// card, which is the N+1 §2.2 refused to build.
func (l *liveNotes) resolveNotes(ctx context.Context, ns []notes.Note) error {
	batches := make([][]notes.SessionLink, 0, len(ns))
	for i := range ns {
		if len(ns[i].Sessions) > 0 {
			batches = append(batches, ns[i].Sessions)
		}
	}
	return l.resolve(ctx, batches...)
}

// resolveOne is the single-note spelling, used by every method returning one Note.
func (l *liveNotes) resolveOne(ctx context.Context, n notes.Note, err error) (notes.Note, error) {
	if err != nil {
		return n, err
	}
	if rerr := l.resolve(ctx, n.Sessions); rerr != nil {
		return notes.Note{}, rerr
	}
	return n, nil
}

// resolveMany is the slice spelling.
func (l *liveNotes) resolveMany(ctx context.Context, ns []notes.Note, err error) ([]notes.Note, error) {
	if err != nil {
		return ns, err
	}
	if rerr := l.resolveNotes(ctx, ns); rerr != nil {
		return nil, rerr
	}
	return ns, nil
}

// --- the link-bearing reads ------------------------------------------------
//
// Each is the same two lines: delegate, then compose. They are spelled out rather
// than generated because Go has no way to wrap an interface method generically, and
// the ledger test below is what keeps the list complete.

func (l *liveNotes) Get(ctx context.Context, id int64) (notes.Note, error) {
	n, err := l.Store.Get(ctx, id)
	return l.resolveOne(ctx, n, err)
}

func (l *liveNotes) Create(ctx context.Context, n notes.Note) (notes.Note, error) {
	out, err := l.Store.Create(ctx, n)
	return l.resolveOne(ctx, out, err)
}

func (l *liveNotes) CreateOnceByTag(ctx context.Context, key string, n notes.Note) (notes.Note, bool, error) {
	out, created, err := l.Store.CreateOnceByTag(ctx, key, n)
	out, err = l.resolveOne(ctx, out, err)
	return out, created, err
}

func (l *liveNotes) Restore(ctx context.Context, id int64) (notes.Note, error) {
	n, err := l.Store.Restore(ctx, id)
	return l.resolveOne(ctx, n, err)
}

func (l *liveNotes) SetStatus(ctx context.Context, id int64, status string) (notes.Note, error) {
	n, err := l.Store.SetStatus(ctx, id, status)
	return l.resolveOne(ctx, n, err)
}

func (l *liveNotes) UpdateNote(ctx context.Context, id int64, patch notes.NoteUpdate) (notes.Note, error) {
	n, err := l.Store.UpdateNote(ctx, id, patch)
	return l.resolveOne(ctx, n, err)
}

func (l *liveNotes) AddTags(ctx context.Context, id int64, tags []string) (notes.Note, error) {
	n, err := l.Store.AddTags(ctx, id, tags)
	return l.resolveOne(ctx, n, err)
}

func (l *liveNotes) RemoveTags(ctx context.Context, id int64, tags []string) (notes.Note, error) {
	n, err := l.Store.RemoveTags(ctx, id, tags)
	return l.resolveOne(ctx, n, err)
}

func (l *liveNotes) List(ctx context.Context) ([]notes.Note, error) {
	ns, err := l.Store.List(ctx)
	return l.resolveMany(ctx, ns, err)
}

func (l *liveNotes) ListByTags(ctx context.Context, tags []string) ([]notes.Note, error) {
	ns, err := l.Store.ListByTags(ctx, tags)
	return l.resolveMany(ctx, ns, err)
}

// ListSummaries carries no threads by contract (it is the no-fan-out read). It is
// overridden anyway: the composition is a no-op on an empty batch, and an override
// that costs nothing is cheaper than a reader having to re-establish that the
// contract still holds.
func (l *liveNotes) ListSummaries(ctx context.Context) ([]notes.Note, error) {
	ns, err := l.Store.ListSummaries(ctx)
	return l.resolveMany(ctx, ns, err)
}

func (l *liveNotes) ListPage(ctx context.Context, f notes.ListFilter) (notes.Page, error) {
	p, err := l.Store.ListPage(ctx, f)
	if err != nil {
		return p, err
	}
	if rerr := l.resolveNotes(ctx, p.Notes); rerr != nil {
		return notes.Page{}, rerr
	}
	return p, nil
}

func (l *liveNotes) SessionsForTask(ctx context.Context, noteID int64) ([]notes.SessionLink, error) {
	links, err := l.Store.SessionsForTask(ctx, noteID)
	if err != nil {
		return links, err
	}
	if rerr := l.resolve(ctx, links); rerr != nil {
		return nil, rerr
	}
	return links, nil
}

// LinkSession composes from the observation the CALLER already made, not from a
// second probe.
//
// 🔴 detailSeen IS THE SAME BIT. It answers "does a transcript record for this
// session exist right now", which is DetailAvailable's definition verbatim; the
// store ORs it into the monotonic stored column and cannot report it back. Probing
// again here would be a second read of the same row in the same request, and the
// two could disagree.
func (l *liveNotes) LinkSession(ctx context.Context, noteID int64, sl notes.SessionLink, detailSeen bool) (notes.SessionLink, bool, error) {
	out, changed, err := l.Store.LinkSession(ctx, noteID, sl, detailSeen)
	if err != nil {
		return out, changed, err
	}
	out.DetailAvailable = detailSeen
	return out, changed, nil
}
