package hermeshost

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE READ PATH UNDER HOSTILE FILES (round-1 audit, #189 🟡2): a symlink must
// never carry a read to a secret, a FIFO must never hang it, and no error may
// quote a file's content.

const secretBody = "SECRETCONTENT-sk-or-v1-feedface"

// noContent fails when err quotes any of body — even its first byte, which
// encoding/json puts in "invalid character 'S' …".
func noContent(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error", what)
	}
	if strings.Contains(err.Error(), secretBody[:1]+"'") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("%s: the error quotes the file: %v", what, err)
	}
}

func TestABreadcrumbSymlinkToASecretIsNeverFollowed(t *testing.T) {
	h := hermestest.New(t)
	h.Decoy()
	dir := filepath.Join(h.Dir, "terminal-sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(h.Dir, "auth.json"), filepath.Join(dir, "tty-dev-pts-7")); err != nil {
		t.Fatal(err)
	}
	stop := inotifyOpens(t, h.Dir)
	_, err := NewHome(h.Dir).ReadBreadcrumb(tty)
	opened := stop()
	noContent(t, "a symlinked breadcrumb", err)
	for _, p := range opened {
		if p == "auth.json" {
			t.Fatal("a breadcrumb symlink opened auth.json")
		}
	}
}

func TestAFIFOBreadcrumbNeverHangsTheRead(t *testing.T) {
	h := hermestest.New(t)
	dir := filepath.Join(h.Dir, "terminal-sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "tty-dev-pts-7"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := NewHome(h.Dir).ReadBreadcrumb(tty); done <- err }()
	select {
	case err := <-done:
		// Refused as what it is — not a regular file — not merely because
		// reading it gave nothing.
		if !errors.Is(err, errNotRegular) {
			t.Fatalf("a FIFO breadcrumb: %v, want errNotRegular", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reading a FIFO breadcrumb hung")
	}
}

func TestAStatusFileSymlinkIsNeverFollowed(t *testing.T) {
	parent := t.TempDir()
	dir := StatusDir(filepath.Join(parent, "status"))
	if err := os.MkdirAll(string(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	id := hermestest.LocalID()
	b, _ := json.Marshal(StatusFile{V: 1, SessionID: id, Status: mirror.StatusIdle, Since: 1})
	if err := os.WriteFile(filepath.Join(parent, "elsewhere.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(parent, "elsewhere.json"), filepath.Join(string(dir), id+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := dir.Read(id); err == nil {
		t.Fatal("a symlinked status file was believed")
	}
}

func TestASymlinkedStateDBIsRefused(t *testing.T) {
	h := hermestest.New(t)
	root := hermestest.LocalID()
	h.Session(root, hermestest.SessionOpts{})
	other := t.TempDir()
	if err := os.Symlink(filepath.Join(h.Dir, "state.db"), filepath.Join(other, "state.db")); err != nil {
		t.Fatal(err)
	}
	st := NewHome(other).OpenState()
	defer st.Close()
	if _, err := st.Snapshot(ctx(t), root); err == nil {
		t.Fatal("a symlinked state.db was read")
	}
}

// No error quotes a file: a breadcrumb, a status file and an allowlist whose
// content is not JSON.
func TestNoErrorQuotesAFile(t *testing.T) {
	h := hermestest.New(t)
	dir := filepath.Join(h.Dir, "terminal-sessions")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "tty-dev-pts-7"), []byte(secretBody), 0o600)
	_, err := NewHome(h.Dir).ReadBreadcrumb(tty)
	noContent(t, "a breadcrumb", err)

	sd := StatusDir(t.TempDir())
	id := hermestest.LocalID()
	_ = os.WriteFile(filepath.Join(string(sd), id+".json"), []byte(secretBody), 0o600)
	_, err = sd.Read(id)
	noContent(t, "a status file", err)

	_ = os.WriteFile(filepath.Join(h.Dir, "shell-hooks-allowlist.json"), []byte(secretBody), 0o600)
	_, err = NewHome(h.Dir).WriteAllowlist(testCommand, time.Now())
	noContent(t, "the allowlist writer", err)
	_, err = NewHome(h.Dir).CheckAllowlist(testCommand)
	noContent(t, "the allowlist check", err)
}

// 🔴 The writer never copies foreign content: only the approvals schema
// survives a write — no unknown top-level key, no unknown entry key.
func TestTheAllowlistWriterCopiesOnlyTheApprovalsSchema(t *testing.T) {
	dir := t.TempDir()
	doc := map[string]any{
		"stolen": secretBody,
		"approvals": []any{
			map[string]any{"event": "pre_tool_call", "command": "/h/guard.sh", "approved_at": "2026-01-01T00:00:00Z",
				"script_mtime_at_approval": nil, "smuggled": secretBody},
			map[string]any{"event": 7, "command": secretBody},
			secretBody,
		},
	}
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, "shell-hooks-allowlist.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHome(dir).WriteAllowlist(testCommand, time.Now()); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(filepath.Join(dir, "shell-hooks-allowlist.json"))
	if strings.Contains(string(out), "SECRET") {
		t.Fatalf("foreign content was copied:\n%s", out)
	}
	if !strings.Contains(string(out), "/h/guard.sh") {
		t.Fatalf("a valid approval was dropped:\n%s", out)
	}
}

func TestTheAllowlistWriterRefusesASymlinkOutsideTheStore(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"approvals":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "shell-hooks-allowlist.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHome(dir).WriteAllowlist(testCommand, time.Now()); err == nil {
		t.Fatal("wrote through a symlink outside the Nix store")
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced")
	}
	if b, _ := os.ReadFile(target); string(b) != `{"approvals":[]}` {
		t.Fatalf("the symlink's target was written: %s", b)
	}
}

// A Home Manager link into the Nix store is replaced by a real file (Hermes
// writes the same file when someone answers [y/N]).
func TestTheAllowlistWriterReplacesAStoreSymlink(t *testing.T) {
	store := t.TempDir()
	prev := nixStore
	nixStore = store + "/"
	t.Cleanup(func() { nixStore = prev })
	target := filepath.Join(store, "allowlist.json")
	if err := os.WriteFile(target, []byte(`{"approvals":[{"event":"pre_tool_call","command":"/h/x.sh"}]}`), 0o444); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "shell-hooks-allowlist.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHome(dir).WriteAllowlist(testCommand, time.Now()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("not replaced by a real 0600 file: %v %v", fi.Mode(), err)
	}
	if b, _ := os.ReadFile(link); !strings.Contains(string(b), "/h/x.sh") {
		t.Fatalf("the store file's approvals were not kept: %s", b)
	}
}

// 🔴 A failed column check is retried (after a backoff), so a state.db that
// was not yet the expected schema recovers without a restart.
func TestASchemaErrorIsRetried(t *testing.T) {
	prev := schemaRetry
	schemaRetry = 10 * time.Millisecond
	t.Cleanup(func() { schemaRetry = prev })
	h := hermestest.New(t)
	h.Exec(`ALTER TABLE session_turn_leases RENAME TO leases_later`)
	root := hermestest.LocalID()
	h.Session(root, hermestest.SessionOpts{})
	st := NewHome(h.Dir).OpenState()
	defer st.Close()
	if _, err := st.Snapshot(ctx(t), root); !errors.Is(err, ErrSchema) {
		t.Fatalf("first read: %v, want ErrSchema", err)
	}
	h.Exec(`ALTER TABLE leases_later RENAME TO session_turn_leases`)
	time.Sleep(30 * time.Millisecond)
	if _, err := st.Snapshot(ctx(t), root); err != nil {
		t.Fatalf("after the table appeared: %v — the schema error is cached", err)
	}
}

// Two hook events in the same millisecond: the later ARRIVAL wins (hooks run
// synchronously in the gateway, so arrival order is event order), and since
// stays strictly increasing.
func TestStatusWritesInTheSameMillisecondAreOrdered(t *testing.T) {
	dir := StatusDir(t.TempDir())
	id := hermestest.LocalID()
	now := time.Now().UnixMilli()
	if err := dir.Write(StatusFile{SessionID: id, Status: mirror.StatusBusy, Since: now}); err != nil {
		t.Fatal(err)
	}
	if err := dir.Write(StatusFile{SessionID: id, Status: mirror.StatusIdle, Since: now}); err != nil {
		t.Fatal(err)
	}
	f, err := dir.Read(id)
	if err != nil || f.Status != mirror.StatusIdle || f.Since <= now {
		t.Fatalf("same-ms write: %+v %v; want the later idle, since > %d", f, err, now)
	}
}

// D1: the hook keeps every status file (no 30-day prune on finalize).
func TestTheHookPrunesNothing(t *testing.T) {
	dir := StatusDir(t.TempDir())
	old := hermestest.LocalID()
	if err := dir.Write(StatusFile{SessionID: old, Status: mirror.StatusIdle, Since: 1}); err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-90 * 24 * time.Hour)
	_ = os.Chtimes(filepath.Join(string(dir), old+".json"), long, long)
	if err := HandleHook(strings.NewReader(string(payload(EventSessionFinalize, hermestest.LocalID(), nil))), dir, time.Now(), Writer{PID: 1, Start: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := dir.Read(old); err != nil {
		t.Fatalf("an old status file was removed: %v", err)
	}
}
