package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient wires a Client at a test server, with a credential and an
// actor, so every test below exercises the real request-building path.
//
// ⚠ THE FIXTURE TOKEN IS SHORT ON PURPOSE. The repository's leak gate refuses
// anything bearer-shaped and 20+ characters, in a fixture as readily as in a
// manifest — and the gate is not to be edited to accommodate a test. A short
// value exercises the same code path; nothing here depends on its length.
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(Config{BaseURL: srv.URL, Token: "tok-fixture", Actor: "muster"})
	if c == nil {
		t.Fatal("New returned nil for a complete configuration")
	}
	return c
}

// TestNewRefusesAnIncompleteConfiguration pins the nil-rather-than-half-built
// rule.
//
// 🔴 EACH CASE OMITS EXACTLY ONE FIELD, AND THE OTHER TWO ARE NON-EMPTY AND
// DISTINCT. A case that left two fields empty would pass with any one of the
// three checks deleted.
func TestNewRefusesAnIncompleteConfiguration(t *testing.T) {
	full := Config{BaseURL: "http://example.test", Token: "tok", Actor: "muster"}
	if New(full) == nil {
		t.Fatal("a complete configuration produced nil, so every case below is vacuous")
	}
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"no base URL", Config{Token: full.Token, Actor: full.Actor}},
		{"no token", Config{BaseURL: full.BaseURL, Actor: full.Actor}},
		{"no actor", Config{BaseURL: full.BaseURL, Token: full.Token}},
		{"whitespace only", Config{BaseURL: "  ", Token: "  ", Actor: "  "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if c := New(tc.cfg); c != nil {
				t.Errorf("New built a client from an incomplete config; it would 401 on every "+
					"call, which is indistinguishable from a router that is down. got %+v", c)
			}
		})
	}
}

// TestEveryCallCarriesTheCredentialAndTheActor is the reason this package
// exists.
//
// 🔴 IT DRIVES EVERY EXPORTED METHOD, DERIVED FROM A TABLE THAT MUST COVER THEM
// ALL. A test that checked one method would pass while a sixth forgot the
// actor header — and the failure mode of a missing actor is a 403 that reads as
// a credential problem.
func TestEveryCallCarriesTheCredentialAndTheActor(t *testing.T) {
	type seen struct {
		auth, token, actor, path, method string
	}
	var got []seen
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, seen{
			auth:   r.Header.Get("Authorization"),
			token:  r.Header.Get(TokenHeader),
			actor:  r.Header.Get(ActorHeader),
			path:   r.URL.Path,
			method: r.Method,
		})
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/gate":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"requestId":"x"}`)) //nolint:errcheck
		case strings.HasPrefix(r.URL.Path, "/api/gate/") && r.Method == http.MethodGet:
			w.Write([]byte(`{"state":"pending"}`)) //nolint:errcheck
		case strings.HasPrefix(r.URL.Path, "/api/sessions/"):
			w.Write([]byte(`{"sessionId":"s","project":"p","cwd":"/c","host":"h"}`)) //nolint:errcheck
		case r.URL.Path == "/api/directories":
			w.Write([]byte(`{"directories":["/a"]}`)) //nolint:errcheck
		default:
			w.Write([]byte(`{}`)) //nolint:errcheck
		}
	})

	ctx := context.Background()
	calls := []struct {
		name string
		fn   func() error
	}{
		{"SessionsExisting", func() error { _, err := c.SessionsExisting(ctx, []string{"s"}); return err }},
		{"SessionMeta", func() error { _, _, err := c.SessionMeta(ctx, "s"); return err }},
		{"PublishEvent", func() error { return c.PublishEvent(ctx, "task.changed", `{"id":"1"}`) }},
		{"MintGate", func() error { _, err := c.MintGate(ctx, GateSpec{Type: "checkpoint"}); return err }},
		{"ReadGate", func() error { _, err := c.ReadGate(ctx, "x"); return err }},
		{"ClearGate", func() error { return c.ClearGate(ctx, "x") }},
		{"Notify", func() error { return c.Notify(ctx, Notification{Type: "task"}) }},
		{"Directories", func() error { _, err := c.Directories(ctx, "q", 5); return err }},
	}
	for _, call := range calls {
		t.Run(call.name, func(t *testing.T) {
			before := len(got)
			if err := call.fn(); err != nil {
				t.Fatalf("%s: %v", call.name, err)
			}
			if len(got) == before {
				t.Fatalf("%s issued NO request, so the header assertions below are vacuous", call.name)
			}
			for _, s := range got[before:] {
				if s.auth != "Bearer tok-fixture" {
					t.Errorf("%s %s: Authorization = %q", s.method, s.path, s.auth)
				}
				if s.token != "tok-fixture" {
					t.Errorf("%s %s: %s = %q", s.method, s.path, TokenHeader, s.token)
				}
				if s.actor != "muster" {
					t.Errorf("%s %s: %s = %q — the receiving door refuses an unattributable "+
						"request, so this is a 403 that reads as a credential problem",
						s.method, s.path, ActorHeader, s.actor)
				}
			}
		})
	}
}

// TestSessionLookupDistinguishesAbsenceFromFailure is the property the
// three-state transcript row is built on.
//
// 🔴 THE 404 AND THE 500 MUST NOT PRODUCE THE SAME ANSWER. Collapsing them is
// the conflation that, upstream, rendered "no transcript recorded" over live
// transcripts for months and read as ordinary retention.
func TestSessionLookupDistinguishesAbsenceFromFailure(t *testing.T) {
	t.Run("404 is an answer", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		live, err := c.SessionsExisting(context.Background(), []string{"a", "b"})
		if err != nil {
			t.Fatalf("a 404 became an error: %v", err)
		}
		if len(live) != 0 {
			t.Errorf("got %v, want no live sessions", live)
		}
	})

	// 🔴 THE 500 CARRIES A VALID JSON BODY, AND THAT IS THE WHOLE POINT OF THIS
	// FIXTURE. The first draft answered 500 with an EMPTY body — and passed for
	// the wrong reason: the JSON decode failed, so the error came from the
	// decoder and the status check below it never executed. A mutation that
	// deleted the status check SURVIVED a green test. Measured, not reasoned
	// about. A well-formed body makes the decode succeed, so only the status
	// check can produce the error this asserts.
	t.Run("500 with a decodable body is a failure, not an absence", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"the database is unreachable"}`)) //nolint:errcheck
		})
		if _, err := c.SessionsExisting(context.Background(), []string{"a"}); err == nil {
			t.Fatal("a 500 was reported as 'this session does not exist'. That renders the " +
				"permanent-looking sentence 'no transcript recorded' over every row of a " +
				"page, silently, on a transient database fault.")
		}
	})

	// The same trap one level down: the single-session read must also refuse.
	t.Run("SessionMeta refuses a decodable 500", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"the database is unreachable"}`)) //nolint:errcheck
		})
		_, ok, err := c.SessionMeta(context.Background(), "a")
		if err == nil {
			t.Fatalf("a 500 came back as ok=%v with no error", ok)
		}
		if ok {
			t.Error("a failed read reported the session as PRESENT")
		}
	})

	// 🔴 AND THE CASE THE TWO ABOVE CANNOT SEE: a 200 whose body does not decode.
	// It must be an error, not an absence — a truncated response from a proxy is
	// exactly the shape that would otherwise read as "no transcript".
	t.Run("a 200 with an undecodable body is a failure", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"sessionId":`)) //nolint:errcheck
		})
		if _, err := c.SessionsExisting(context.Background(), []string{"a"}); err == nil {
			t.Fatal("a truncated 200 was reported as 'this session does not exist'")
		}
	})

	t.Run("401 names the credential", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
		_, err := c.SessionsExisting(context.Background(), []string{"a"})
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("a 401 did not surface as ErrUnauthorized: %v", err)
		}
	})
}

// TestSessionsExistingIssuesOneRequestPerDistinctID pins the deduplication, and
// with it the claim in the package doc that the batched port costs one call per
// BOARD READ rather than one per card.
func TestSessionsExistingIssuesOneRequestPerDistinctID(t *testing.T) {
	n := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if strings.HasSuffix(r.URL.Path, "/live") {
			w.Write([]byte(`{"sessionId":"live"}`)) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	live, err := c.SessionsExisting(context.Background(),
		[]string{"live", "dead", "live", "", "dead", "live"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("issued %d requests for 2 distinct non-empty ids (6 entries with duplicates "+
			"and a blank); want 2", n)
	}
	if !live["live"] || live["dead"] {
		t.Errorf("liveness map = %v, want only \"live\" true", live)
	}
}

// TestNilClientRefusesEveryCall pins the nil-safety the package doc promises,
// and pins it as an ERROR rather than a zero value.
func TestNilClientRefusesEveryCall(t *testing.T) {
	var c *Client
	ctx := context.Background()
	checks := []struct {
		name string
		err  error
	}{
		{"SessionsExisting", func() error { _, e := c.SessionsExisting(ctx, []string{"a"}); return e }()},
		{"SessionMeta", func() error { _, _, e := c.SessionMeta(ctx, "a"); return e }()},
		{"PublishEvent", c.PublishEvent(ctx, "n", "d")},
		{"MintGate", func() error { _, e := c.MintGate(ctx, GateSpec{}); return e }()},
		{"ReadGate", func() error { _, e := c.ReadGate(ctx, "x"); return e }()},
		{"ClearGate", c.ClearGate(ctx, "x")},
		{"Notify", c.Notify(ctx, Notification{})},
		{"Directories", func() error { _, e := c.Directories(ctx, "", 1); return e }()},
	}
	for _, ch := range checks {
		if !errors.Is(ch.err, ErrNotConfigured) {
			t.Errorf("%s on a nil client returned %v, want ErrNotConfigured. A zero value "+
				"here is indistinguishable from 'the router said no'.", ch.name, ch.err)
		}
	}
	if c.Configured() {
		t.Error("a nil client reports itself configured")
	}
}

// TestMintGateRefusesEveryNonCreatedAnswer pins the rule that a card which
// could not be FILED is never an approval.
func TestMintGateRefusesEveryNonCreatedAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"bad type", http.StatusBadRequest, `{"error":"that request type cannot be minted"}`},
		{"no queue", http.StatusServiceUnavailable, `{"error":"this server has no request queue"}`},
		{"server error", http.StatusInternalServerError, `{"error":"boom"}`},
		{"created but no id", http.StatusCreated, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body)) //nolint:errcheck
			})
			id, err := c.MintGate(context.Background(), GateSpec{Type: "checkpoint"})
			if err == nil {
				t.Fatalf("got id %q and no error; a checkpoint that could not be filed must "+
					"never resolve to an id a caller will poll", id)
			}
			if id != "" {
				t.Errorf("an error came back with a non-empty id %q", id)
			}
		})
	}
}

// TestReadGateRefusesAnUnknownState pins the refusal to guess.
//
// 🔴 DEFAULTING AN UNKNOWN STATE TO `pending` WOULD POLL FOREVER; DEFAULTING IT
// TO `gone` WOULD ABANDON A LIVE CARD. Both are wrong in a way nothing reports,
// so the client says it does not know.
func TestReadGateRefusesAnUnknownState(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"state":"deliberating"}`)) //nolint:errcheck
	})
	if _, err := c.ReadGate(context.Background(), "x"); err == nil {
		t.Fatal("an unrecognised state was accepted")
	}
	// Positive control: the three known states ARE accepted, so the refusal
	// above is about the vocabulary and not about the decode path.
	for _, state := range []string{GateDecided, GatePending, GateGone} {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"state":"` + state + `"}`)) //nolint:errcheck
		})
		if _, err := c.ReadGate(context.Background(), "x"); err != nil {
			t.Errorf("state %q was refused: %v", state, err)
		}
	}
}

// TestGateSpecUsesTheWireSpelling pins the one field whose Go name and JSON
// name differ, because the receiving service refuses the other spelling.
func TestGateSpecUsesTheWireSpelling(t *testing.T) {
	var body string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 512)
		n, _ := r.Body.Read(b)
		body = string(b[:n])
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"requestId":"x"}`)) //nolint:errcheck
	})
	if _, err := c.MintGate(context.Background(),
		GateSpec{Type: "agent-permission", Project: "muster-agent/bold-hawk", SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	// The type and project travel verbatim: the router allowlists the type and
	// matches an auto-approve window on the exact project.
	for _, want := range []string{`"type":"agent-permission"`, `"project":"muster-agent/bold-hawk"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the gate body does not carry %s: %s", want, body)
		}
	}
	if !strings.Contains(body, `"sessionId":"sess-1"`) {
		t.Errorf("the gate body does not carry `sessionId`: %s\n"+
			"The receiving service refuses a bare `session` on a new producer struct, "+
			"because it has exactly one meaning for that word.", body)
	}
	if strings.Contains(body, `"session":`) {
		t.Errorf("the gate body carries a bare `session` key: %s", body)
	}
}

// TestBaseURLTrailingSlashIsTolerated pins a configuration mistake that would
// otherwise produce a double slash and a 404 on every call.
func TestBaseURLTrailingSlashIsTolerated(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Write([]byte(`{}`)) //nolint:errcheck
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL + "/", Token: "tok", Actor: "muster"})
	if c == nil {
		t.Fatal("New returned nil")
	}
	if err := c.PublishEvent(context.Background(), "n", "d"); err != nil {
		t.Fatal(err)
	}
	if path != "/api/events/publish" {
		t.Errorf("path = %q, want /api/events/publish — a trailing slash on the base URL "+
			"produced a doubled separator", path)
	}
}
