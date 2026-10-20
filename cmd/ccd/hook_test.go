package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The recorded payloads (pinned CLI, deliberately invalid token) decode into the
// fields ccd branches on. If the CLI renames one, this is where it shows.
func TestRecordedHookPayloadsCarryTheFieldsCcdReads(t *testing.T) {
	read := func(name string) hookEvent {
		var ev hookEvent
		if err := json.Unmarshal(fixture(t, name), &ev); err != nil {
			t.Fatal(err)
		}
		return ev
	}
	ss := read("hook_SessionStart_2.1.296.json")
	if ss.Event != "SessionStart" || ss.SessionID == "" || ss.Source != "startup" {
		t.Fatalf("SessionStart: %+v", ss)
	}
	ups := read("hook_UserPromptSubmit_2.1.296.json")
	if ups.Event != "UserPromptSubmit" || ups.PromptID != fixturePromptID || ups.TranscriptPath == "" ||
		ups.Prompt != "para one\n\npara two with \"quotes\" and $HOME" {
		t.Fatalf("UserPromptSubmit: %+v", ups)
	}
	sf := read("hook_StopFailure_2.1.296.json")
	if sf.Event != "StopFailure" || sf.PromptID != fixturePromptID || sf.Error != "authentication_failed" ||
		!strings.Contains(sf.LastAssistantMessage, "401") {
		t.Fatalf("StopFailure: %+v", sf)
	}
	// The prompt id the hooks carry is the promptId the transcript records carry.
	if !extract(t, fixture(t, "turn_auth_failed_2.1.296.jsonl"), sf.PromptID).Found {
		t.Fatal("the hooks' prompt_id does not locate the turn in the transcript")
	}
}

func TestRunHookForwardsTheEventPayload(t *testing.T) {
	var mu sync.Mutex
	var gotPath string
	var gotBody []byte
	var gotAuth string
	unixHookTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(204)
	}))
	payload := fixture(t, "hook_StopFailure_2.1.296.json")
	var stderr bytes.Buffer
	if rc := runHook([]string{"StopFailure"}, bytes.NewReader(payload), &stderr); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/hook/StopFailure" || !bytes.Equal(gotBody, payload) || gotAuth != "Bearer "+testHookSecret {
		t.Fatalf("path %q auth %q body %q", gotPath, gotAuth, gotBody)
	}
}

// 🔴 A DOWN ccd MUST COST THE SESSION NOTHING: exit 2 from a Stop hook would keep
// the turn running, from UserPromptSubmit it would refuse the prompt.
func TestRunHookExitsZeroWhenCcdIsUnreachable(t *testing.T) {
	t.Setenv(envHookSocket, filepath.Join(t.TempDir(), "nothing-listens-here.sock"))
	t.Setenv(envHookSecret, testHookSecret)
	var stderr bytes.Buffer
	if rc := runHook([]string{"Stop"}, strings.NewReader(`{}`), &stderr); rc != 0 {
		t.Fatalf("rc = %d with ccd down", rc)
	}
	if stderr.Len() == 0 {
		t.Fatal("an unreachable ccd was not reported on stderr")
	}
	if rc := runHook(nil, strings.NewReader(`{}`), &stderr); rc != 0 {
		t.Fatalf("rc = %d on a usage error", rc)
	}
}

func TestRunHookIsInertWhenDisabled(t *testing.T) {
	called := false
	unixHookTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	t.Setenv("CCD_HOOK_DISABLED", "1")
	if rc := runHook([]string{"Stop"}, strings.NewReader(`{}`), io.Discard); rc != 0 || called {
		t.Fatalf("rc=%d called=%v", rc, called)
	}
	// Positive control: without the variable the same call does reach the server.
	os.Unsetenv("CCD_HOOK_DISABLED")
	if rc := runHook([]string{"Stop"}, strings.NewReader(`{}`), io.Discard); rc != 0 || !called {
		t.Fatalf("control: rc=%d called=%v", rc, called)
	}
}

func TestHookLogCapturesRawPayloadsOnlyWhenSet(t *testing.T) {
	srv, _ := newScripted(t, cliScript{})
	hp := hooks(t, srv)
	payload := fixture(t, "hook_StopFailure_2.1.296.json")
	post := func() { hp.mustPost(t, "StopFailure", string(payload)) }
	post() // HookLog unset: nothing written anywhere
	log := t.TempDir() + "/hooks.jsonl"
	srv.cfg.HookLog = log
	post()
	post()
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	line := append(bytes.TrimSpace(payload), '\n')
	if !bytes.Equal(got, append(append([]byte{}, line...), line...)) {
		t.Fatalf("hook log = %q", got)
	}
}
