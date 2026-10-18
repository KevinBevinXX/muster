package main

import (
	"context"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/askq"
)

// TestTheBackgroundLoopsSweepTheQuestionStore (PR #130 audit 🟢3): the
// AskUserQuestion store's expiry runs as one of the server's background
// loops — with no database, no request and no mirror plugin — so a question
// whose hook died is withdrawn (firing the change callback that drops its
// window's "waiting"), and the loop ends with the server's context.
func TestTheBackgroundLoopsSweepTheQuestionStore(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	a := &app{logger: logger, srv: api.New(nil, api.AuthConfig{}, logger)}
	store := a.srv.Questions()
	var mu sync.Mutex
	now := time.Now()
	store.SetNow(func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	withdrawn := make(chan askq.Entry, 4)
	store.OnChange(func(e askq.Entry) {
		if e.State == askq.Withdrawn {
			withdrawn <- e
		}
	})
	qs := []askq.Question{{Question: "Q?", Options: []askq.Option{{Label: "A"}, {Label: "B"}}}}
	if _, err := store.Open(askq.Key{Owner: 1, Source: "mirror", Scope: "h", Session: "s", ToolUseID: "t"}, qs, time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackgroundLoops(ctx)
	mu.Lock()
	now = now.Add(askq.PollGrace + time.Second)
	mu.Unlock()
	select {
	case e := <-withdrawn:
		if e.Reason != askq.ReasonStopped {
			t.Fatalf("withdrawn as %q, want stopped", e.Reason)
		}
	case <-time.After(askq.SweepEvery + 5*time.Second):
		t.Fatal("no background loop swept the question store")
	}
}
