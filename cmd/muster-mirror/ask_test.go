package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		var req struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": req.ID, "state": "pending"})
	})
	mux.HandleFunc("GET /mirror/ask/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": r.PathValue("id"), "state": "answered", "answers": answers})
	})
	mux.HandleFunc("DELETE /mirror/ask/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": r.PathValue("id"), "state": "withdrawn"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestAskPrintsTheAnswersAndExitsZero(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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
	// A HOME with no recorded config and no default token file.
	t.Setenv("HOME", t.TempDir())
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
	t.Setenv("HOME", t.TempDir())
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

// TestAskNeedsNoFlagsOnAHostWhoseMirrorRecordedItsConfig (audit round 0): the
// settings entry is just `muster-mirror ask`. The daemon records its URL and
// token file (hookconfig.go); with no flag and no environment the hook uses
// exactly those.
func TestAskNeedsNoFlagsOnAHostWhoseMirrorRecordedItsConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var seen []string
	srv := fakeAskMuster(t, map[string]string{"Which color do you prefer?": "Blue", "Which pets do you have?": "Fish"}, &seen)
	tok := filepath.Join(home, "elsewhere", "tok")
	if err := os.MkdirAll(filepath.Dir(tok), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tok, []byte("mmt_recorded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeHookConfig(filepath.Join(home, ".muster", "mirror-hook.json"), hookConfig{URL: srv.URL, AllowHTTP: true, TokenFile: tok}); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	runAsk(nil, env(nil), bytes.NewReader(askPayload(t)), &out, &errb)
	if !strings.Contains(out.String(), `"answers"`) || len(seen) != 1 || seen[0] != "Bearer mmt_recorded" {
		t.Fatalf("no-flag hook: stdout %q, tokens sent %q, stderr %s", out.String(), seen, errb.String())
	}
}

// TestAskDefaultsToTheHostsTokenPath: with no recorded token file the hook
// reads ~/.muster/mirror-token — the path the host's service uses.
func TestAskDefaultsToTheHostsTokenPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var seen []string
	srv := fakeAskMuster(t, map[string]string{"Which color do you prefer?": "Blue", "Which pets do you have?": "Fish"}, &seen)
	if err := os.MkdirAll(filepath.Join(home, ".muster"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".muster", "mirror-token"), []byte("mmt_default\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The URL is the recorded one (with no recorded token file): the token
	// is the host's default file.
	if err := writeHookConfig(filepath.Join(home, ".muster", "mirror-hook.json"), hookConfig{URL: srv.URL, AllowHTTP: true}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runAsk(nil, env(nil), bytes.NewReader(askPayload(t)), &out, &bytes.Buffer{})
	if len(seen) != 1 || seen[0] != "Bearer mmt_default" {
		t.Fatalf("tokens sent %q, want the default path's", seen)
	}
}

// TestAskSaysWhichTokenFileItCannotRead: one stderr line naming the path
// (invisible to the picker, in Claude Code's debug log), nothing on stdout.
func TestAskSaysWhichTokenFileItCannotRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := writeHookConfig(filepath.Join(home, ".muster", "mirror-hook.json"), hookConfig{URL: "https://m.example"}); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := runAsk(nil, env(nil), bytes.NewReader(askPayload(t)), &out, &errb)
	want := filepath.Join(home, ".muster", "mirror-token")
	if code != 0 || out.Len() != 0 || !strings.Contains(errb.String(), "cannot read the mirror token file") || !strings.Contains(errb.String(), "mirror-token") {
		t.Fatalf("exit %d stdout %q stderr %q (want a line naming %s)", code, out.String(), errb.String(), want)
	}
}

// TestTheDaemonRecordsItsConfigForTheHook: the host daemon, started as its
// service starts it (URL and token file from the environment), records them
// where the hook reads them — the token file as an absolute path, and no token.
func TestTheDaemonRecordsItsConfigForTheHook(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized) // ends the daemon's Run
	}))
	defer refuse.Close()
	old := newTmuxBackend
	newTmuxBackend = func(string, string, string) mirrorhost.Backend { return listBackend{} }
	defer func() { newTmuxBackend = old }()
	tok := filepath.Join(home, ".muster", "mirror-token")
	_ = os.MkdirAll(filepath.Dir(tok), 0o700)
	if err := os.WriteFile(tok, []byte("mmt_"+strings.Repeat("a", 43)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The daemon runs until its context ends: end it once it has recorded.
	cfgPath := filepath.Join(home, ".muster", "mirror-hook.json")
	ctx, cancel := context.WithCancel(context.Background())
	oldCtx := runContext
	runContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	defer func() { runContext = oldCtx }()
	go func() {
		for i := 0; i < 1000; i++ {
			if _, err := os.Stat(cfgPath); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	if rc, _, errs := runWith(t, map[string]string{envURL: refuse.URL, envAllowHTTP: "1", envTokenFile: tok, envHost: "laptop", envWriteHookConfig: "1"}); rc == 2 {
		t.Fatalf("the daemon did not start: %s", errs)
	}
	c, err := readHookConfig(filepath.Join(home, ".muster", "mirror-hook.json"))
	if err != nil || c.URL != refuse.URL || !c.AllowHTTP || c.TokenFile != tok {
		t.Fatalf("recorded %+v (%v), want url %s, allowHttp, token file %s", c, err, refuse.URL, tok)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".muster", "mirror-hook.json"))
	if strings.Contains(string(b), "mmt_") {
		t.Fatalf("the recorded config holds the token: %s", b)
	}
}
