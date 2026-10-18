package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// PR #130 round-2 audit: the recorded token goes only to the recorded URL,
// and the record is written only by a daemon told to, atomically.

// TestAnExplicitURLNeverGetsTheRecordedToken (🟡1): a hook given a URL by flag
// or environment, with no token of its own, must not send the RECORDED token
// (nor the default file's) there — it fails open. The control: the same URL
// with an explicit token file is contacted with that token.
func TestAnExplicitURLNeverGetsTheRecordedToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var seen []string
	srv := fakeAskMuster(t, map[string]string{"Which color do you prefer?": "Blue", "Which pets do you have?": "Fish"}, &seen)
	dir := filepath.Join(home, ".muster")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	recordedTok := filepath.Join(dir, "recorded-token")
	_ = os.WriteFile(recordedTok, []byte("mmt_recorded\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "mirror-token"), []byte("mmt_default\n"), 0o600)
	if err := writeHookConfig(filepath.Join(dir, "mirror-hook.json"), hookConfig{URL: "https://recorded.example", TokenFile: recordedTok}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		args []string
		env  map[string]string
	}{
		"-url":              {[]string{"-url", srv.URL, "-allow-http"}, nil},
		"MUSTER_MIRROR_URL": {nil, map[string]string{envURL: srv.URL, envAllowHTTP: "1"}},
	} {
		var out, errb bytes.Buffer
		if code := runAsk(c.args, env(c.env), bytes.NewReader(askPayload(t)), &out, &errb); code != 0 || out.Len() != 0 {
			t.Errorf("%s: exit %d stdout %q", name, code, out.String())
		}
		if !strings.Contains(errb.String(), "token") {
			t.Errorf("%s: stderr does not say a token is needed: %q", name, errb.String())
		}
	}
	if len(seen) != 0 {
		t.Fatalf("a URL not from the record was sent the recorded/default token: %q", seen)
	}
	explicit := filepath.Join(home, "explicit-token")
	_ = os.WriteFile(explicit, []byte("mmt_explicit\n"), 0o600)
	runAsk([]string{"-url", srv.URL, "-allow-http", "-token-file", explicit}, env(nil), bytes.NewReader(askPayload(t)), &bytes.Buffer{}, &bytes.Buffer{})
	if len(seen) != 1 || seen[0] != "Bearer mmt_explicit" {
		t.Fatalf("the control: tokens sent %q, want the explicit one", seen)
	}
}

// TestADaemonRecordsOnlyWhenToldTo (🟡2): a daemon started without
// MUSTER_MIRROR_WRITE_HOOK_CONFIG (an ad-hoc or development one) leaves the
// hook record alone; the control is TestTheDaemonRecordsItsConfigForTheHook.
func TestADaemonRecordsOnlyWhenToldTo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var dialed atomic.Int32
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dialed.Add(1)
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer refuse.Close()
	old := newTmuxBackend
	newTmuxBackend = func(string, string, string) mirrorhost.Backend { return listBackend{} }
	defer func() { newTmuxBackend = old }()
	tok := filepath.Join(home, "tok")
	_ = os.WriteFile(tok, []byte("mmt_"+strings.Repeat("a", 43)+"\n"), 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	oldCtx := runContext
	runContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	defer func() { runContext = oldCtx }()
	// End the daemon once it has dialled: by then it is past where it records.
	go func() {
		for i := 0; i < 2000 && dialed.Load() == 0; i++ {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	runWith(t, map[string]string{envURL: refuse.URL, envAllowHTTP: "1", envTokenFile: tok, envHost: "laptop"})
	if dialed.Load() == 0 {
		t.Fatal("the daemon never dialled: this case tests nothing")
	}
	if _, err := os.Stat(filepath.Join(home, ".muster", "mirror-hook.json")); !os.IsNotExist(err) {
		t.Fatalf("a daemon not told to record wrote the hook record (stat: %v)", err)
	}
}

// TestConcurrentRecordsNeverInterleave (🟡2): many daemons recording at once
// each succeed, the record left is one of theirs whole, and no temp file is
// left behind.
func TestConcurrentRecordsNeverInterleave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror-hook.json")
	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- writeHookConfig(path, hookConfig{URL: fmt.Sprintf("https://m%d.example", i), TokenFile: strings.Repeat("/t", 200)})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent record failed: %v", err)
		}
	}
	c, err := readHookConfig(path)
	if err != nil || !strings.HasPrefix(c.URL, "https://m") {
		t.Fatalf("the record left is not one whole write: %+v %v", c, err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("files left beside the record: %v", names)
	}
}
