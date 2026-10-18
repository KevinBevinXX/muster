//go:build tmuxit

package mirrorhost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askdrive"
	"github.com/ZacxDev/muster/internal/askq"
)

// ANSWERING A PICKER IN A REAL tmux PANE: the keys go in by `paste-buffer -S`
// with muster's READ-ONLY viewer attached (with which `send-keys` is refused),
// and land as keys — arrows, Enter, digits, text — in a program reading a raw
// tty. The program is askdrive's FakePicker (internal/askdrive/fakepicker): the
// picker as measured on Claude Code 2.1.289, where no Claude Code runs.

// pickerSession starts a session whose pane runs the fake picker on qs, and
// returns its pane and the file its result lands in.
func (r *tmuxRig) pickerSession(name string, qs []askq.Question) (pane, result string) {
	r.t.Helper()
	dir := r.t.TempDir()
	bin := filepath.Join(dir, "fakepicker")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/ZacxDev/muster/internal/askdrive/fakepicker").CombinedOutput(); err != nil {
		r.t.Fatalf("build the fake picker: %v: %s", err, out)
	}
	qf := filepath.Join(dir, "questions.json")
	b, _ := json.Marshal(qs)
	if err := os.WriteFile(qf, b, 0o600); err != nil {
		r.t.Fatal(err)
	}
	result = filepath.Join(dir, "result.json")
	r.run("new-session", "-d", "-s", name, "-x", "100", "-y", "40", "sh", "-c", "stty raw -echo; exec "+bin+" "+qf+" "+result)
	pane = strings.TrimSpace(r.run("display-message", "-p", "-t", "="+name+":", "#{pane_id}"))
	waitFor(r.t, "the picker on screen", func() bool { return strings.Contains(r.pane(name), "Enter to select") })
	return pane, result
}

func readResult(t *testing.T, path string) (map[string]string, bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var res struct {
		Answers   map[string]string `json:"answers"`
		Cancelled bool              `json:"cancelled"`
	}
	if err := json.Unmarshal(b, &res); err != nil || res.Cancelled {
		t.Fatalf("the picker's result: %s (%v)", b, err)
	}
	return res.Answers, true
}

// TestTmuxAnswerAskTypesThroughAReadOnlyViewer: three questions — own words,
// multi-select with own words, single-select — typed into the pane with a
// read-only client attached; the picker submits exactly those answers.
func TestTmuxAnswerAskTypesThroughAReadOnlyViewer(t *testing.T) {
	r := newTmuxRig(t)
	qs := []askq.Question{
		{Question: "Which color do you prefer?", Header: "Color", Options: []askq.Option{{Label: "Red"}, {Label: "Blue"}, {Label: "Green"}}},
		{Question: "Which pets do you have?", Header: "Pets", MultiSelect: true, Options: []askq.Option{{Label: "Cat"}, {Label: "Dog"}, {Label: "Fish"}}},
		{Question: "What should the project be called?", Header: "Name", Options: []askq.Option{{Label: "Alpha"}, {Label: "Beta"}}},
	}
	cs := []askq.Choice{{Other: "teal, mostly"}, {Picks: []int{1}, Other: "a hamster"}, {Picks: []int{1}}}
	pane, result := r.pickerSession("ask", qs)
	b := Tmux{Socket: r.sock}
	term, err := b.Attach(context.Background(), "ask", true, 100, 40)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	go func() { _, _ = io.Copy(io.Discard, term) }()
	waitFor(t, "the read-only viewer", func() bool { return strings.Contains(r.run("list-clients", "-F", "#{client_flags}"), "read-only") })
	// The control: send-keys IS refused with the viewer attached.
	if out, err := r.cmd("send-keys", "-t", pane, "x").CombinedOutput(); err == nil || !strings.Contains(string(out), "read-only") {
		t.Fatalf("send-keys with a read-only viewer: %v %s — the premise of paste-buffer -S does not hold", err, out)
	}
	if err := b.AnswerAsk(context.Background(), pane, qs, cs); err != nil {
		t.Fatalf("AnswerAsk: %v\n%s", err, r.pane("ask"))
	}
	var got map[string]string
	waitFor(t, "the picker's result", func() bool { var ok bool; got, ok = readResult(t, result); return ok })
	if want := askdrive.Answers(qs, cs); !reflect.DeepEqual(got, want) {
		t.Fatalf("the picker submitted %v, want %v", got, want)
	}
}

// TestTmuxAnswerAskNeverTypesIntoATouchedPicker: someone has ticked a box in
// the terminal; the answer is refused before any key, and nothing is submitted.
func TestTmuxAnswerAskNeverTypesIntoATouchedPicker(t *testing.T) {
	r := newTmuxRig(t)
	qs := []askq.Question{{Question: "Which pets do you have?", Header: "Pets", MultiSelect: true, Options: []askq.Option{{Label: "Cat"}, {Label: "Dog"}}}}
	pane, result := r.pickerSession("touched", qs)
	b := Tmux{Socket: r.sock}
	if err := (tmuxPane{t: b, id: pane}).Send(context.Background(), "2"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the tick", func() bool { return strings.Contains(r.pane("touched"), "[✔] Dog") })
	before := r.pane("touched")
	err := b.AnswerAsk(context.Background(), pane, qs, []askq.Choice{{Picks: []int{0}}})
	if !errors.Is(err, askdrive.ErrNotUntouched) {
		t.Fatalf("a touched picker: %v", err)
	}
	if after := r.pane("touched"); after != before {
		t.Fatalf("keys were typed into a touched picker:\n%s", after)
	}
	if _, ok := readResult(t, result); ok {
		t.Fatal("a touched picker was submitted")
	}
}

// TestTmuxNoKeyWhileSomeoneTypes: a person types into the session through an
// ordinary (read-write) client — a key the picker ignores, so the picker
// itself is untouched — and the answer is refused before any key: their
// next key could land at once. muster's own read-only viewer is attached
// throughout and does not count. The control: once the client has been idle
// past the window, the same answer types.
func TestTmuxNoKeyWhileSomeoneTypes(t *testing.T) {
	r := newTmuxRig(t)
	qs := []askq.Question{{Question: "Which pets do you have?", Header: "Pets", MultiSelect: true, Options: []askq.Option{{Label: "Cat"}, {Label: "Dog"}}}}
	pane, result := r.pickerSession("typing", qs)
	b := Tmux{Socket: r.sock}
	ro, err := b.Attach(context.Background(), "typing", true, 100, 40)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	go func() { _, _ = io.Copy(io.Discard, ro) }()
	rw, err := b.Attach(context.Background(), "typing", false, 100, 40)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	go func() { _, _ = io.Copy(io.Discard, rw) }()
	waitFor(t, "both clients", func() bool { return strings.Count(r.run("list-clients", "-F", "#{client_readonly}"), "\n") == 2 })
	// Let the clients' attach-time activity age out, then type.
	time.Sleep(typingWithin + 1500*time.Millisecond)
	if p := (tmuxPane{t: b, id: pane}); func() bool { busy, _ := p.RecentInput(context.Background()); return busy }() {
		t.Fatal("an idle client counts as typing")
	}
	if _, err := rw.Write([]byte("z")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the keystroke", func() bool { busy, _ := (tmuxPane{t: b, id: pane}).RecentInput(context.Background()); return busy })
	before := r.pane("typing")
	err = b.AnswerAsk(context.Background(), pane, qs, []askq.Choice{{Picks: []int{0}}})
	if !errors.Is(err, askdrive.ErrNotUntouched) || !strings.Contains(err.Error(), "typed in the terminal") {
		t.Fatalf("an answer while someone types: %v", err)
	}
	if after := r.pane("typing"); after != before {
		t.Fatalf("keys were typed while someone was typing:\n%s", after)
	}
	// The control: idle again, the answer types — with a read-only viewer
	// that attached just now (its activity is this second): a viewer cannot
	// type, so it does not count.
	time.Sleep(typingWithin + 1500*time.Millisecond)
	ro2, err := b.Attach(context.Background(), "typing", true, 100, 40)
	if err != nil {
		t.Fatal(err)
	}
	defer ro2.Close()
	go func() { _, _ = io.Copy(io.Discard, ro2) }()
	waitFor(t, "the new viewer", func() bool { return strings.Count(r.run("list-clients", "-F", "#{client_readonly}"), "\n") == 3 })
	if err := b.AnswerAsk(context.Background(), pane, qs, []askq.Choice{{Picks: []int{0}}}); err != nil {
		t.Fatalf("an answer once nobody types: %v\n%s", err, r.pane("typing"))
	}
	waitFor(t, "the picker's result", func() bool { _, ok := readResult(t, result); return ok })
}

// TestTmuxLeftoverBuffersAreDropped: a buffer a dead muster-mirror left
// between load-buffer and paste-buffer is deleted before the next drive; an
// operator's own buffers are not. And this tmux has paste-buffer -S.
func TestTmuxLeftoverBuffersAreDropped(t *testing.T) {
	r := newTmuxRig(t)
	qs := []askq.Question{{Question: "Which color?", Header: "Color", Options: []askq.Option{{Label: "Red"}, {Label: "Blue"}}}}
	pane, _ := r.pickerSession("buffers", qs)
	r.run("set-buffer", "-b", askBufferPrefix+"0123456789ab", "left behind")
	r.run("set-buffer", "-b", "operators-own", "keep me")
	// One of this process's own keys in flight (another pane's drive): kept.
	live := askBufferPrefix + "1111111111ff"
	r.run("set-buffer", "-b", live, "in flight")
	liveBuffers.Lock()
	liveBuffers.names[live] = true
	liveBuffers.Unlock()
	defer func() {
		liveBuffers.Lock()
		delete(liveBuffers.names, live)
		liveBuffers.Unlock()
	}()
	b := Tmux{Socket: r.sock}
	if !b.PastesRawKeys(context.Background()) {
		t.Fatal("this tmux reports no paste-buffer -S")
	}
	if err := b.AnswerAsk(context.Background(), pane, qs, []askq.Choice{{Picks: []int{1}}}); err != nil {
		t.Fatal(err)
	}
	got := r.run("list-buffers", "-F", "#{buffer_name}")
	if strings.Contains(got, askBufferPrefix+"0123456789ab") || !strings.Contains(got, "operators-own") || !strings.Contains(got, live) {
		t.Fatalf("buffers after a drive: %q", got)
	}
}
