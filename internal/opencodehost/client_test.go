package opencodehost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// TestTheClientReadsSessionsStatusMessagesAndEvents: every read against the
// fake — root sessions across pages, the running set, a session's messages
// oldest first, the event stream — and every request it made was a GET.
func TestTheClientReadsSessionsStatusMessagesAndEvents(t *testing.T) {
	srv := opencodetest.New(fixturePassword)
	defer srv.Close()
	for i := 0; i < 130; i++ {
		srv.PutSession(fmt.Sprintf("ses_fixture%03d", i), fmt.Sprintf("title %d", i), "")
	}
	srv.PutSession("ses_fixturechild", "a subagent", "ses_fixture000")
	tok := srv.Issue()
	cl, err := NewClient(srv.URL, func() string { return tok })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ss, err := cl.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 130 || ss[0].ID != "ses_fixture129" || ss[129].ID != "ses_fixture000" {
		t.Fatalf("%d sessions, first %+v", len(ss), ss[0])
	}
	for _, s := range ss {
		if s.ParentID != "" {
			t.Fatalf("a child session was listed: %+v", s)
		}
	}
	srv.SetStatus("ses_fixture007", "busy")
	run, err := cl.Running(ctx)
	if err != nil || len(run) != 1 || !run["ses_fixture007"] {
		t.Fatalf("running %v %v", run, err)
	}
	for i := 0; i < 5; i++ {
		srv.PutMessage("ses_fixture007", map[string]any{"id": fmt.Sprintf("msg_fixture%02d", i), "type": "user", "text": fmt.Sprintf("prompt %d", i),
			"time": map[string]any{"created": 1700000000000 + i}})
	}
	ms, err := cl.Messages(ctx, "ses_fixture007", 3)
	if err != nil || len(ms) != 3 || ms[0].Text != "prompt 2" || ms[2].Text != "prompt 4" {
		t.Fatalf("messages %+v %v", ms, err)
	}
	if _, err := cl.Messages(ctx, "../api/pair", 3); err == nil {
		t.Fatal("a path through the session id was accepted")
	}
	// The event stream: frames in order, each naming its session.
	ectx, cancel := context.WithCancel(ctx)
	got := make(chan Event, 8)
	connected := make(chan struct{})
	go func() {
		_ = cl.Events(ectx, func() { close(connected) }, func(e Event) { got <- e })
	}()
	<-connected
	for srv.Subscribers() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	srv.SetStatus("ses_fixture007", "idle")
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-got:
			if e.Type == "server.connected" {
				continue
			}
			if e.Type != "session.status" || e.SessionID() != "ses_fixture007" || e.Status() != "idle" {
				t.Fatalf("event %+v", e)
			}
			cancel()
			for _, r := range srv.Requests() {
				if r.Method != "GET" || !r.Authorized {
					t.Fatalf("request %+v", r)
				}
			}
			return
		case <-deadline:
			t.Fatal("no event")
		}
	}
}

// TestARefusedTokenIsErrUnauthorizedOnEveryRead: an expired token, and one
// the server no longer accepts because its password was rotated, read
// nothing: every read, the event stream included, says ErrUnauthorized.
func TestARefusedTokenIsErrUnauthorizedOnEveryRead(t *testing.T) {
	srv := opencodetest.New(fixturePassword)
	defer srv.Close()
	srv.PutSession("ses_fixture1", "one", "")
	expired := srv.IssueAt(time.Now().Add(-time.Minute))
	valid := srv.Issue()
	srv.Rotate("rotated-fixture-password")
	for name, tok := range map[string]string{"expired": expired, "revoked": valid} {
		cl, _ := NewClient(srv.URL, func() string { return tok })
		ctx := context.Background()
		if _, err := cl.Sessions(ctx); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: Sessions %v", name, err)
		}
		if _, err := cl.Running(ctx); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: Running %v", name, err)
		}
		if _, err := cl.Messages(ctx, "ses_fixture1", 5); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: Messages %v", name, err)
		}
		if err := cl.Events(ctx, nil, func(Event) {}); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: Events %v", name, err)
		}
	}
}

// TestTheClientRefusesAForeignServerBeforeSendingAnything: the credential
// goes to a loopback server only.
func TestTheClientRefusesAForeignServerBeforeSendingAnything(t *testing.T) {
	for _, u := range []string{"http://192.0.2.1:13420", "http://opencode.example", "https://127.0.0.1:13420", "ftp://127.0.0.1:1"} {
		if _, err := NewClient(u, func() string { return "x" }); err == nil {
			t.Errorf("%s was accepted", u)
		}
	}
	for _, u := range []string{"http://127.0.0.1:13420", "http://localhost:4096", "http://[::1]:4096", "http://127.0.0.1:13420/"} {
		if _, err := NewClient(u, func() string { return "x" }); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	if !strings.Contains(CheckLocal("http://192.0.2.1:1").Error(), "loopback") {
		t.Fatal("the refusal does not say why")
	}
}
