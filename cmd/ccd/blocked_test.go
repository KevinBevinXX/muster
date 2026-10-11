package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestAPermissionPromptMarksTheSessionAwaitingApprovalUntilItMovesOn(t *testing.T) {
	for _, clearing := range []string{"PostToolUse", "Stop", "StopFailure", "UserPromptSubmit", "SessionStart", "SessionEnd"} {
		t.Run(clearing, func(t *testing.T) {
			s := newServer(serverConfig{}, nil, newAuthTracker())
			s.blocked = &blockedReporter{ch: make(chan bool, 1)}
			s.onHook(hookEvent{Event: "Notification", NotificationType: "idle_prompt"})
			if s.awaitingApproval {
				t.Fatal("an idle_prompt notification set awaiting approval")
			}
			s.onHook(hookEvent{Event: "Notification", NotificationType: "permission_prompt"})
			if !s.awaitingApproval {
				t.Fatal("a permission_prompt notification did not set awaiting approval")
			}
			if got := <-s.blocked.ch; !got {
				t.Fatal("reported blocked=false on the prompt")
			}
			s.onHook(hookEvent{Event: clearing})
			if s.awaitingApproval {
				t.Fatalf("%s did not clear awaiting approval", clearing)
			}
			select {
			case got := <-s.blocked.ch:
				if got {
					t.Fatal("reported blocked=true on the clear")
				}
			default:
				t.Fatal("the clear was not reported")
			}
		})
	}
}

// Hooks that do not change the state report nothing (no POST per tool call).
func TestOnlyATransitionIsReported(t *testing.T) {
	s := newServer(serverConfig{}, nil, newAuthTracker())
	s.blocked = &blockedReporter{ch: make(chan bool, 1)}
	s.onHook(hookEvent{Event: "PostToolUse"})
	s.onHook(hookEvent{Event: "Stop"})
	select {
	case v := <-s.blocked.ch:
		t.Fatalf("reported %v with no transition", v)
	default:
	}
}

func TestTheReporterPostsTheStateWithTheAgentsOwnTokenAndHeartbeats(t *testing.T) {
	var mu sync.Mutex
	var got []bool
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/agent/blocked" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		var v struct {
			Blocked bool `json:"blocked"`
		}
		_ = json.Unmarshal(b, &v)
		mu.Lock()
		got = append(got, v.Blocked)
		tokens = append(tokens, r.Header.Get("X-Muster-Token"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	r := newBlockedReporter(srv.URL, "agent-own-token")
	r.heartbeat = 20 * time.Millisecond
	r.logf = func(string, ...any) {}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.run(ctx); close(done) }()
	r.set(true)
	time.Sleep(90 * time.Millisecond) // the report plus heartbeats
	r.set(false)
	time.Sleep(60 * time.Millisecond) // no heartbeat while clear
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(got) < 3 || !got[0] || got[len(got)-1] {
		t.Fatalf("posted %v: want true, at least one heartbeat true, then false last", got)
	}
	for _, tok := range tokens {
		if tok != "agent-own-token" {
			t.Fatalf("posted with token %q", tok)
		}
	}
}
