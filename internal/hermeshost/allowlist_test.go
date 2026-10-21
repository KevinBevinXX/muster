package hermeshost

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const testCommand = "/nix/store/abc-muster-mirror/bin/muster-mirror hermes-hook"

type entry struct {
	Event   string `json:"event"`
	Command string `json:"command"`
}

func readEntries(t *testing.T, dir string) (map[string]any, []entry) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "shell-hooks-allowlist.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var d struct{ Approvals []entry }
	_ = json.Unmarshal(b, &d)
	return doc, d.Approvals
}

func pairs(es []entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Event+" "+e.Command)
	}
	sort.Strings(out)
	return out
}

func ours(command string) []string {
	var out []string
	for _, ev := range HookEvents {
		out = append(out, ev+" "+command)
	}
	return out
}

func TestWriteAllowlistCreatesExactlyOurPairs(t *testing.T) {
	dir := t.TempDir()
	changed, err := NewHome(dir).WriteAllowlist(testCommand, time.Now())
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	_, es := readEntries(t, dir)
	want := ours(testCommand)
	sort.Strings(want)
	if got := pairs(es); !reflect.DeepEqual(got, want) {
		t.Fatalf("pairs = %v\nwant %v", got, want)
	}
	fi, _ := os.Stat(filepath.Join(dir, "shell-hooks-allowlist.json"))
	if fi.Mode().Perm() != 0o600 || !fi.Mode().IsRegular() {
		t.Fatalf("mode %v, want a regular 0600 file", fi.Mode())
	}
	// Idempotent: a second run changes nothing and does not rewrite.
	before, _ := os.ReadFile(filepath.Join(dir, "shell-hooks-allowlist.json"))
	changed, err = NewHome(dir).WriteAllowlist(testCommand, time.Now().Add(time.Hour))
	after, _ := os.ReadFile(filepath.Join(dir, "shell-hooks-allowlist.json"))
	if err != nil || changed || string(before) != string(after) {
		t.Fatalf("second run: changed=%v err=%v", changed, err)
	}
	// No temp file left behind.
	m, _ := filepath.Glob(filepath.Join(dir, ".*.tmp"))
	if len(m) != 0 {
		t.Fatalf("left %v", m)
	}
}

// It keeps every other APPROVAL, drops an OLDER muster-mirror's entries, a
// duplicate of ours, and everything outside the approvals schema (another
// key, an entry that is not an approval).
func TestWriteAllowlistKeepsOthersAndReplacesAnOlderBinary(t *testing.T) {
	dir := t.TempDir()
	old := "/nix/store/old-muster-mirror/bin/muster-mirror hermes-hook"
	doc := map[string]any{
		"approvals": []any{
			map[string]any{"event": "pre_tool_call", "command": "~/.hermes/agent-hooks/guard.sh && echo", "approved_at": "2026-01-01T00:00:00Z"},
			map[string]any{"event": "pre_llm_call", "command": old},
			map[string]any{"event": "on_session_end", "command": testCommand, "approved_at": "keep-me"},
			map[string]any{"event": "on_session_end", "command": testCommand, "approved_at": "dup"},
			"not an object",
		},
		"note": "someone's key",
	}
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, "shell-hooks-allowlist.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHome(dir).WriteAllowlist(testCommand, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, es := readEntries(t, dir)
	if _, ok := got["note"]; ok || len(got) != 1 {
		t.Fatalf("a key outside the approvals schema was copied: %v", got)
	}
	want := append(ours(testCommand), "pre_tool_call ~/.hermes/agent-hooks/guard.sh && echo")
	sort.Strings(want)
	if p := pairs(es); !reflect.DeepEqual(p, want) {
		t.Fatalf("pairs = %q\nwant %q", p, want)
	}
	// The entry we already had kept its approved_at.
	for _, raw := range got["approvals"].([]any) {
		if e, ok := raw.(map[string]any); ok && e["event"] == "on_session_end" && e["approved_at"] != "keep-me" {
			t.Fatalf("our existing entry was rewritten: %v", e)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "shell-hooks-allowlist.json"))
	if !json.Valid(raw) || !strings.Contains(string(raw), "guard.sh && echo") {
		t.Fatalf("not written with && intact: %s", raw)
	}
}

func TestWriteAllowlistRefusesAnUnreadableFile(t *testing.T) {
	for _, body := range []string{"{not json", `[]`, `{"approvals": "x"}`, `null`} {
		dir := t.TempDir()
		p := filepath.Join(dir, "shell-hooks-allowlist.json")
		_ = os.WriteFile(p, []byte(body), 0o600)
		if _, err := NewHome(dir).WriteAllowlist(testCommand, time.Now()); !errors.Is(err, ErrAllowlistUnreadable) {
			t.Errorf("%q: %v, want ErrAllowlistUnreadable", body, err)
		}
		if b, _ := os.ReadFile(p); string(b) != body {
			t.Errorf("%q was replaced with %q", body, b)
		}
	}
}

func TestWriteAllowlistRefusesABadCommand(t *testing.T) {
	for _, c := range []string{"muster-mirror hermes-hook", "/bin/sh -c x", "/a b/muster-mirror hermes-hook",
		"/x/muster-mirror hermes-hook; rm -rf ~", "/x/muster-mirror ask", "/x/other hermes-hook"} {
		if _, err := NewHome(t.TempDir()).WriteAllowlist(c, time.Now()); err == nil {
			t.Errorf("%q was accepted", c)
		}
	}
}

// 🔴 IT TAKES HERMES'S LOCK: while another process holds
// shell-hooks-allowlist.json.lock (Hermes recording a [y/N] answer), it waits,
// and the answer written under that lock survives.
func TestWriteAllowlistWaitsForHermessLock(t *testing.T) {
	dir := t.TempDir()
	lock, err := os.OpenFile(filepath.Join(dir, "shell-hooks-allowlist.json.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := NewHome(dir).WriteAllowlist(testCommand, time.Now())
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("wrote while Hermes held its lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	// "Hermes" records an approval under its lock, then releases it.
	b, _ := json.Marshal(map[string]any{"approvals": []any{map[string]any{"event": "pre_tool_call", "command": "/h/answer.sh"}}})
	_ = os.WriteFile(filepath.Join(dir, "shell-hooks-allowlist.json"), b, 0o600)
	_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	_ = lock.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_, es := readEntries(t, dir)
	want := append(ours(testCommand), "pre_tool_call /h/answer.sh")
	sort.Strings(want)
	if p := pairs(es); !reflect.DeepEqual(p, want) {
		t.Fatalf("the answer written under the lock was lost: %v", p)
	}
}

func TestCheckAllowlist(t *testing.T) {
	dir := t.TempDir()
	h := NewHome(dir)
	c, err := h.CheckAllowlist(testCommand)
	if err != nil || c.OK() || len(c.Missing) != len(HookEvents) {
		t.Fatalf("no file: %+v %v", c, err)
	}
	if _, err := h.WriteAllowlist(testCommand, time.Now()); err != nil {
		t.Fatal(err)
	}
	if c, err := h.CheckAllowlist(testCommand); err != nil || !c.OK() || len(c.Other) != 0 {
		t.Fatalf("after the write: %+v %v", c, err)
	}
	other := "/other/muster-mirror hermes-hook"
	c, err = h.CheckAllowlist(other)
	if err != nil || c.OK() || len(c.Missing) != len(HookEvents) || len(c.Other) != len(HookEvents) {
		t.Fatalf("a different binary: %+v %v", c, err)
	}
}
