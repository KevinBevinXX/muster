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

// TestTheBackgroundLoopsSweepTheQuestionStore: the AskUserQuestion store's
// expiry runs as one of the server's background loops — with no database, no
// request and no mirror plugin — so an open question no result was seen for
// expires (firing the change callback that takes it off the attention queue),
// and the loop ends with the server's context.
func TestTheBackgroundLoopsSweepTheQuestionStore(t *testing.T) {
	old := askSweepEvery
	askSweepEvery = 10 * time.Millisecond
	defer func() { askSweepEvery = old }()
	logger := log.New(io.Discard, "", 0)
	a := &app{logger: logger, srv: api.New(nil, api.AuthConfig{}, logger)}
	store := a.srv.Questions()
	var mu sync.Mutex
	now := time.Now()
	store.SetNow(func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	expired := make(chan askq.Entry, 4)
	store.OnChange(func(e askq.Entry) {
		if e.State == askq.Expired {
			expired <- e
		}
	})
	qs := []askq.Question{{Question: "Q?", Options: []askq.Option{{Label: "A"}, {Label: "B"}}}}
	if _, err := store.Open(askq.Key{Owner: 1, Source: "mirror", Scope: "h", Session: "s", ToolUseID: "t"}, qs, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackgroundLoops(ctx)
	mu.Lock()
	now = now.Add(askq.MaxOpen + time.Second)
	mu.Unlock()
	select {
	case <-expired:
	case <-time.After(5 * time.Second):
		t.Fatal("no background loop swept the question store")
	}
}
