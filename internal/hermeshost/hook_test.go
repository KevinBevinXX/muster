package hermeshost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
	"github.com/ZacxDev/muster/internal/mirror"
)

// payload is a hook payload shaped as Hermes's _payload_fields writes it.
func payload(event, session string, extra map[string]any) []byte {
	if extra == nil {
		extra = map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"hook_event_name": event, "tool_name": nil, "tool_input": nil,
		"session_id": session, "cwd": "/w", "profile": "default", "extra": extra})
	return b
}

// pr is payload as the hook's stdin.
func pr(event, session string, extra map[string]any) io.Reader {
	return bytes.NewReader(payload(event, session, extra))
}

func TestHookWritesEachEventsStatus(t *testing.T) {
	id := hermestest.LocalID()
	for event, want := range map[string]string{
		EventPreLLMCall:         mirror.StatusBusy,
		EventHumanInputResolved: mirror.StatusBusy,
		EventSessionEnd:         mirror.StatusIdle,
		EventSessionFinalize:    mirror.StatusIdle,
	} {
		dir := StatusDir(t.TempDir())
		now := time.Now()
		if err := HandleHook(pr(event, id, nil), dir, now, Writer{PID: 42, Start: "777"}); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		f, err := dir.Read(id)
		if err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		if f.Status != want || f.Since != now.UnixMilli() || f.WriterPID != 42 || f.WriterStart != "777" || f.Kind != "" {
			t.Errorf("%s: wrote %+v, want %s", event, f, want)
		}
	}
}

// 🔴 A waiting file holds the request's kind and id — never its prompt (the
// approval's command), which Hermes's payload carries in extra.prompt.
func TestHookRecordsAWaitAndNeverItsPrompt(t *testing.T) {
	dir := StatusDir(t.TempDir())
	id := hermestest.LocalID()
	secret := "rm -rf ./victim SECRET-sk-or-v1-abc"
	extra := map[string]any{"kind": "approval", "request_id": "0123456789abcdef0123456789abcdef", "prompt": secret,
		"session_key": "agent:main:cli:" + secret, "platform": "cli"}
	if err := HandleHook(pr(EventHumanInputRequest, id, extra), dir, time.Now(), Writer{PID: 1, Start: "1"}); err != nil {
		t.Fatal(err)
	}
	f, err := dir.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	if f.Status != mirror.StatusWaiting || f.Kind != "approval" || f.RequestID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("wrote %+v", f)
	}
	raw, _ := os.ReadFile(filepath.Join(string(dir), id+".json"))
	if strings.Contains(string(raw), "rm -rf") || strings.Contains(string(raw), "SECRET") {
		t.Fatalf("the status file holds the prompt: %s", raw)
	}
	// An unknown kind writes nothing.
	if err := HandleHook(pr(EventHumanInputRequest, id, map[string]any{"kind": "password"}), dir, time.Now().Add(time.Second), Writer{}); !errors.Is(err, ErrIgnored) {
		t.Fatalf("unknown kind: %v", err)
	}
}

// 🔴 THE SESSION ID BECOMES A FILE NAME: anything the wire's pattern does not
// accept is ignored, and nothing is written anywhere.
func TestHookIgnoresAnInvalidSessionID(t *testing.T) {
	parent := t.TempDir()
	dir := StatusDir(filepath.Join(parent, "status"))
	for _, id := range []string{"", "../escape", "a/b", "..", ".hidden", "-x", "x y", "x\x00y",
		strings.Repeat("a", 129), "local-é", "‮local"} {
		err := HandleHook(pr(EventPreLLMCall, id, nil), dir, time.Now(), Writer{PID: 1, Start: "1"})
		if !errors.Is(err, ErrIgnored) {
			t.Errorf("id %q: %v, want ErrIgnored", id, err)
		}
	}
	// Nothing at all was created: not the directory, not a file beside it.
	ents, _ := os.ReadDir(parent)
	if len(ents) != 0 {
		t.Fatalf("an ignored hook wrote %v", ents)
	}
	// The longest valid id (128) is accepted: the bound is the wire's.
	long := "a" + strings.Repeat("b", 127)
	if err := HandleHook(pr(EventPreLLMCall, long, nil), dir, time.Now(), Writer{PID: 1, Start: "1"}); err != nil {
		t.Fatalf("a 128-byte id: %v", err)
	}
}

// The DAEMON's reads take ids from state.db's lineage, not from the hook: an
// id that is not one (a crafted sessions row, "../…") never reaches a path,
// even when a file sits where it would point.
func TestStatusDirRefusesAnIDThatIsNotOne(t *testing.T) {
	parent := t.TempDir()
	dir := StatusDir(filepath.Join(parent, "status"))
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(string(dir), 0o700))
	// A valid-looking status file OUTSIDE the directory, where "../outside"
	// would lead.
	b, _ := json.Marshal(StatusFile{V: 1, SessionID: "../outside", Status: mirror.StatusIdle, Since: 1})
	must(os.WriteFile(filepath.Join(parent, "outside.json"), b, 0o600))
	// The positive control: a valid id's file in the directory IS seen opened.
	valid := hermestest.LocalID()
	must(dir.Write(StatusFile{SessionID: valid, Status: mirror.StatusIdle, Since: 1}))
	stop := inotifyOpens(t, parent)
	if _, err := dir.Read("../outside"); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Read(../outside) = %v, want a refusal of the id", err)
	}
	if _, ok := dir.Newest(context.Background(), []string{"../outside"}); ok {
		t.Fatal("Newest followed ../outside")
	}
	if _, err := dir.Read(valid); err != nil {
		t.Fatal(err)
	}
	opened := stop()
	for _, p := range opened {
		if p == "outside.json" {
			t.Fatalf("a read with the id ../outside opened %s", p)
		}
	}
	if len(opened) == 0 {
		t.Fatal("instrument check: the watch saw no open at all")
	}
	if err := dir.Write(StatusFile{SessionID: "../escape", Status: mirror.StatusBusy, Since: 2}); err == nil {
		t.Fatal("Write(../escape) was accepted")
	}
	if _, err := os.Stat(filepath.Join(parent, "escape.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Write escaped the directory: %v", err)
	}
}

func TestHookIgnoresOtherEvents(t *testing.T) {
	dir := StatusDir(t.TempDir())
	for _, ev := range []string{"pre_tool_call", "post_llm_call", "on_session_start", ""} {
		if err := HandleHook(pr(ev, hermestest.LocalID(), nil), dir, time.Now(), Writer{}); !errors.Is(err, ErrIgnored) {
			t.Errorf("%q: %v", ev, err)
		}
	}
	if err := HandleHook(strings.NewReader("not json"), dir, time.Now(), Writer{}); err == nil {
		t.Error("a non-JSON payload was accepted")
	}
}

// Monotonic: since only goes up, and the later arrival is the file (a clock
// stepped back cannot make the earlier status the newer one).
func TestStatusWriteIsMonotonic(t *testing.T) {
	dir := StatusDir(t.TempDir())
	id := hermestest.LocalID()
	now := time.Now()
	if err := dir.Write(StatusFile{SessionID: id, Status: mirror.StatusIdle, Since: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if err := dir.Write(StatusFile{SessionID: id, Status: mirror.StatusBusy, Since: now.Add(-time.Second).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	f, _ := dir.Read(id)
	if f.Status != mirror.StatusBusy || f.Since != now.UnixMilli()+1 {
		t.Fatalf("a later arrival with an earlier clock: %+v, want busy at %d", f, now.UnixMilli()+1)
	}
	fi, err := os.Stat(filepath.Join(string(dir), id+".json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v, %v", fi.Mode(), err)
	}
}

func TestStatusDirIsCreatedPrivate(t *testing.T) {
	dir := StatusDir(filepath.Join(t.TempDir(), "hermes-status"))
	if err := dir.Write(StatusFile{SessionID: hermestest.LocalID(), Status: mirror.StatusIdle, Since: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(string(dir))
	if err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode %v, %v", di.Mode(), err)
	}
}

// The lock wait is bounded: a held lock makes the write fail within about
// lockWait, never hang the gateway.
func TestStatusWriteNeverBlocksOnAHeldLock(t *testing.T) {
	dir := StatusDir(t.TempDir())
	unlock, err := lockFile(filepath.Join(string(dir), ".lock"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	start := time.Now()
	err = dir.Write(StatusFile{SessionID: hermestest.LocalID(), Status: mirror.StatusBusy, Since: time.Now().UnixMilli()})
	if err == nil {
		t.Fatal("wrote through a held lock")
	}
	if d := time.Since(start); d > 3*lockWait {
		t.Fatalf("waited %v", d)
	}
}

func TestNewestKeepsTheNewestOfTheLineage(t *testing.T) {
	dir := StatusDir(t.TempDir())
	a, b := hermestest.LocalID(), hermestest.ChildID()
	now := time.Now()
	_ = dir.Write(StatusFile{SessionID: a, Status: mirror.StatusIdle, Since: now.UnixMilli()})
	_ = dir.Write(StatusFile{SessionID: b, Status: mirror.StatusBusy, Since: now.Add(time.Second).UnixMilli()})
	f, ok := dir.Newest(context.Background(), []string{a, b, "missing"})
	if !ok || f.SessionID != b {
		t.Fatalf("Newest = %+v %v", f, ok)
	}
	if _, ok := dir.Newest(context.Background(), []string{"missing"}); ok {
		t.Fatal("found a status for a session with none")
	}
}
