package focus

import (
	"context"
	"log"
	"time"
)

// RetentionSweepInterval is how often the retention sweep runs.
const RetentionSweepInterval = time.Hour

// Retainer keeps mirrored sessions' focus only while the session is seen:
// each pass marks every ENABLED owner's visible mirrored sessions seen
// (Store.Touch, as that owner), then deletes every owner's mirrored rows not
// seen for Retention (Store.PruneAll, under the system scope) — their history
// does NOT go with them: it is kept for its own DefaultHistoryRetention, and
// then pruned (Store.PruneHistory).
//
// 🔴 IT IS ITS OWN LOOP, NOT PART OF THE REFRESH PASS. The refresh pass runs
// only with a summarizer and MUSTER_FOCUS_INTERVAL > 0, and only for the owners
// Owners returns; tying the delete to it left a disabled owner's rows — and
// every row, with the pass off — kept forever. The sweep needs neither: a row
// nothing marks seen ages out.
type Retainer struct {
	Store    Store
	Sessions interface {
		Candidates(ctx context.Context) ([]Candidate, error)
	}
	// AsOwner is ctx acting as owner (internal/api's focusPrincipal).
	AsOwner func(ctx context.Context, owner int64) context.Context
	// Retention is how long a mirrored session's focus is kept after it was
	// last seen (MUSTER_FOCUS_RETENTION); <= 0 is DefaultRetention.
	Retention time.Duration
	Now       func() time.Time
	Logger    *log.Logger
}

func (r *Retainer) retention() time.Duration {
	if r.Retention > 0 {
		return r.Retention
	}
	return DefaultRetention
}

func (r *Retainer) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Retainer) logf(format string, args ...any) {
	if r.Logger != nil {
		r.Logger.Printf(format, args...)
	}
}

// Pass marks what is seen, then prunes, and returns how many FOCUS rows it
// deleted (the history rows it deletes are logged).
// A failure to mark one owner's sessions is logged and the prune still runs:
// a row it then deletes was last seen more than Retention ago anyway.
func (r *Retainer) Pass(ctx context.Context) (int, error) {
	owners, err := r.Store.Owners(ctx)
	if err != nil {
		r.logf("focus: retention: owners: %v", err)
	}
	for _, o := range owners {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		octx := r.AsOwner(ctx, o)
		cands, err := r.Sessions.Candidates(octx)
		if err != nil {
			r.logf("focus: retention: owner %d: sessions: %v", o, err)
			continue
		}
		var mirrors []Subject
		for _, c := range cands {
			if c.Subject.Kind == KindMirror {
				mirrors = append(mirrors, c.Subject)
			}
		}
		if err := r.Store.Touch(octx, mirrors); err != nil {
			r.logf("focus: retention: owner %d: mark seen: %v", o, err)
		}
	}
	now := r.now()
	n, err := r.Store.PruneAll(ctx, now.Add(-r.retention()))
	if err != nil {
		return 0, err
	}
	if n > 0 {
		r.logf("focus: retention: deleted %d mirrored session(s) not seen for %s", n, r.retention())
	}
	h, err := r.Store.PruneHistory(ctx, now.Add(-DefaultHistoryRetention))
	if err != nil {
		return n, err
	}
	if h > 0 {
		r.logf("focus: retention: deleted %d focus history row(s) older than %s", h, DefaultHistoryRetention)
	}
	return n, nil
}

// Run passes now and then every interval until ctx ends (interval <= 0:
// RetentionSweepInterval).
func (r *Retainer) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = RetentionSweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := r.Pass(ctx); err != nil && ctx.Err() == nil {
			r.logf("focus: retention: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
