package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `muster-mirror ask` as the CLI runs it: it reads the hook payload on stdin
// and ALWAYS exits 0, printing only answers muster says a person gave.

func askPayload(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../internal/askq/testdata/pretooluse-askuserquestion.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// fakeAskMuster answers every question with answers at the first poll.
func fakeAskMuster(t *testing.T, answers map[string]string, seen *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mirror/ask", func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"q1","state":"pending"}`))
	})
	mux.HandleFunc("GET /mirror/ask/q1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "q1", "state": "answered", "answers": answers})
	})
	mux.HandleFunc("DELETE /mirror/ask/q1", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestAskPrintsTheAnswersAndExitsZero(t *testing.T) {
	var seen []string
	srv := fakeAskMuster(t, map[string]string{"Which color do you prefer?": "Blue", "Which pets do you have?": "Fish"}, &seen)
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("mmt_secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := runAsk([]string{"-url", srv.URL, "-allow-http", "-token-file", tok, "-timeout", "5s"}, env(nil), bytes.NewReader(askPayload(t)), &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(seen) != 1 || seen[0] != "Bearer mmt_secret" {
		t.Fatalf("the token sent: %q", seen)
	}
	var got struct {
		HSO struct {
			Event        string `json:"hookEventName"`
			Decision     string `json:"permissionDecision"`
			UpdatedInput struct {
				Answers map[string]string `json:"answers"`
			} `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not the hook output: %q (%v; stderr %s)", out.String(), err, errb.String())
	}
	if got.HSO.Event != "PreToolUse" || got.HSO.Decision != "allow" || got.HSO.UpdatedInput.Answers["Which pets do you have?"] != "Fish" {
		t.Fatalf("output %s", out.String())
	}
}

// TestAskPrintsNothingWithoutASafeConfiguration: no URL, no token, a plain
// http:// URL without -allow-http, or an unreadable token file — nothing on
// stdout (the picker), exit 0, and muster never contacted.
func TestAskPrintsNothingWithoutASafeConfiguration(t *testing.T) {
	var seen []string
	srv := fakeAskMuster(t, map[string]string{"Which color do you prefer?": "Blue", "Which pets do you have?": "Fish"}, &seen)
	cases := map[string]struct {
		args []string
		env  map[string]string
	}{
		"no url":                  {nil, map[string]string{envToken: "mmt_x"}},
		"no token":                {[]string{"-url", srv.URL, "-allow-http"}, nil},
		"http without allow-http": {[]string{"-url", srv.URL}, map[string]string{envToken: "mmt_x"}},
		"missing token file":      {[]string{"-url", srv.URL, "-allow-http", "-token-file", "/nonexistent/token"}, nil},
		"a bad flag":              {[]string{"-nope"}, nil},
	}
	for name, c := range cases {
		var out, errb bytes.Buffer
		if code := runAsk(c.args, env(c.env), bytes.NewReader(askPayload(t)), &out, &errb); code != 0 || out.Len() != 0 {
			t.Errorf("%s: exit %d, stdout %q", name, code, out.String())
		}
	}
	if len(seen) != 0 {
		t.Fatalf("muster was contacted %d time(s) without a safe configuration", len(seen))
	}
	// The control: the env alone (as a systemd-less hook command might set
	// it) is a safe configuration.
	var out bytes.Buffer
	runAsk(nil, env(map[string]string{envURL: srv.URL, envAllowHTTP: "1", envToken: "mmt_x"}), bytes.NewReader(askPayload(t)), &out, &bytes.Buffer{})
	if !strings.Contains(out.String(), `"answers"`) || len(seen) != 1 {
		t.Fatalf("the env-configured control printed %q (contacted %d)", out.String(), len(seen))
	}
}

func TestRunDispatchesAsk(t *testing.T) {
	var out, errb bytes.Buffer
	old := stdin
	stdin = strings.NewReader(`{"tool_name":"Bash"}`)
	defer func() { stdin = old }()
	if code := run([]string{"ask"}, env(nil), &out, &errb); code != 0 || out.Len() != 0 {
		t.Fatalf("muster-mirror ask: exit %d, stdout %q", code, out.String())
	}
	// Proof it reached ask, not the host daemon (which would demand -url).
	if strings.Contains(errb.String(), "-url (") {
		t.Fatalf("run treated ask as the daemon: %s", errb.String())
	}
}
