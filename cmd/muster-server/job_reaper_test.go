package main

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/jobs"
)

// reapCounter is a jobs.Store that records reaper passes and nothing else.
type reapCounter struct {
	jobs.Store
	passes chan time.Time
}

func (r *reapCounter) ReapOverdue(_ context.Context, now time.Time) (int64, error) {
	select {
	case r.passes <- now:
	default:
	}
	return 0, nil
}

// TestTheBackgroundLoopsRunTheJobReaper: the job reaper is one of the server's
// background loops — its first pass at start (boot), then on its interval —
// with no database pool in the app, since it keys on the job store alone. It
// replaced a one-shot sweep in buildApp that failed every unfinished job at
// boot, which was wrong while the other pod of a rolling update still ran jobs.
func TestTheBackgroundLoopsRunTheJobReaper(t *testing.T) {
	old := jobReapEvery
	jobReapEvery = 10 * time.Millisecond
	defer func() { jobReapEvery = old }()
	logger := log.New(io.Discard, "", 0)
	srv := api.New(nil, api.AuthConfig{}, logger)
	store := &reapCounter{passes: make(chan time.Time, 8)}
	srv.UseExtensions(api.Extensions{Jobs: store})
	a := &app{logger: logger, srv: srv}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackgroundLoops(ctx)
	for i := 0; i < 2; i++ {
		select {
		case <-store.passes:
		case <-time.After(5 * time.Second):
			t.Fatalf("reaper pass %d never came", i+1)
		}
	}
}
