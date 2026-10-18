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

// requireRawPasteTmux fails the test, saying why, on a tmux older than
// MinRawPasteTmux: muster-mirror cannot type answers with one (no
// `paste-buffer -S`), so nothing here could pass for a reason worth having.
func requireRawPasteTmux(t *testing.T) {
	t.Helper()
	out, err := exec.Command("tmux", "-V").Output()
	if err != nil {
		t.Fatalf("tmux -V: %v", err)
	}
	ok, known := tmuxAtLeast(string(out), MinRawPasteTmux)
	if !known || !ok {
		t.Fatalf("%s has no `paste-buffer -S` (added in tmux %s), which muster-mirror types answers with; run this suite with tmux %s or later (CI installs it from the flake's nixpkgs)", strings.TrimSpace(string(out)), MinRawPasteTmux, MinRawPasteTmux)
	}
}

// pickerSession starts a session whose pane runs the fake picker on qs, and
// returns its pane and the file its result lands in.
func (r *tmuxRig) pickerSession(name string, qs []askq.Question) (pane, result string) {
	r.t.Helper()
	requireRawPasteTmux(r.t)
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
	// The premise, version-dependent (tmux >= MinRawPasteTmux, checked by
	// pickerSession): with the viewer attached — the only client, so the one
	// tmux takes as current — send-keys IS refused. (tmux 3.4 lets it through,
	// and has no paste-buffer -S either.)
	if out, err := r.cmd("send-keys", "-t", pane, "x").CombinedOutput(); err == nil || !strings.Contains(string(out), "read-only") {
		v, _ := exec.Command("tmux", "-V").Output()
		t.Fatalf("send-keys with a read-only viewer on %s: %v %s — the premise of typing by paste-buffer -S does not hold on this tmux", strings.TrimSpace(string(v)), err, out)
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

// typingRig is a picker session with muster's read-only viewer and an
// operator's read-write client attached, tmux's focus events and mouse on (as
// an operator's tmux.conf may have them).
func typingRig(t *testing.T, name string, qs []askq.Question) (r *tmuxRig, b Tmux, pane, result string, rw Terminal) {
	r = newTmuxRig(t)
	pane, result = r.pickerSession(name, qs)
	r.run("set-option", "-g", "focus-events", "on")
	r.run("set-option", "-g", "mouse", "on")
	b = Tmux{Socket: r.sock}
	ro, err := b.Attach(context.Background(), name, true, 100, 40)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ro.Close() })
	go func() { _, _ = io.Copy(io.Discard, ro) }()
	rw, err = b.Attach(context.Background(), name, false, 100, 40)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rw.Close() })
	go func() { _, _ = io.Copy(io.Discard, rw) }()
	waitFor(t, "both clients", func() bool { return strings.Count(r.run("list-clients", "-F", "#{client_readonly}"), "\n") == 2 })
	return r, b, pane, result, rw
}

// TestTmuxFocusAndMouseDoNotStopADrive: while an answer is typed, the
// operator's client sends focus-in events and mouse moves (switching to the
// terminal to watch). They move tmux's input clock — asserted — but change
// nothing on screen, so the drive completes. And a read-only viewer that has
// just attached does not count as input at all.
func TestTmuxFocusAndMouseDoNotStopADrive(t *testing.T) {
	qs := []askq.Question{{Question: "Which pets do you have?", Header: "Pets", MultiSelect: true, Options: []askq.Option{{Label: "Cat"}, {Label: "Dog"}, {Label: "Fish"}}}}
	r, b, pane, result, rw := typingRig(t, "focus", qs)
	p := tmuxPane{t: b, id: pane}
	time.Sleep(2 * time.Second) // the attach-time activity is now old
	before, _ := p.LastInput(context.Background())
	// A viewer attaching now is not input.
	ro2, err := b.Attach(context.Background(), "focus", true, 100, 40)
	if err != nil {
		t.Fatal(err)
	}
	defer ro2.Close()
	go func() { _, _ = io.Copy(io.Discard, ro2) }()
	waitFor(t, "the new viewer", func() bool { return strings.Count(r.run("list-clients", "-F", "#{client_readonly}"), "\n") == 3 })
	if last, _ := p.LastInput(context.Background()); !last.Equal(before) {
		t.Fatalf("a read-only viewer attaching moved the input clock: %v -> %v", before, last)
	}
	done := make(chan error, 1)
	go func() { done <- b.AnswerAsk(context.Background(), pane, qs, []askq.Choice{{Picks: []int{0, 2}}}) }()
	stop := time.After(2 * time.Second)
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
events:
	for {
		select {
		case <-stop:
			break events
		case <-tick.C:
			_, _ = rw.Write([]byte("\x1b[I"))          // focus in
			_, _ = rw.Write([]byte("\x1b[<35;10;10M")) // mouse move
		}
	}
	if last, _ := p.LastInput(context.Background()); !last.After(before) {
		t.Fatalf("the focus and mouse events never moved the input clock (%v): this test saw nothing", last)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a drive with focus and mouse events: %v\n%s", err, r.pane("focus"))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the drive never finished")
	}
	var got map[string]string
	waitFor(t, "the picker's result", func() bool { var ok bool; got, ok = readResult(t, result); return ok })
	if want := askdrive.Answers(qs, []askq.Choice{{Picks: []int{0, 2}}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("the picker submitted %v, want %v", got, want)
	}
}

// TestTmuxAKeystrokeMidDriveStopsIt: the operator ticks Dog through their own
// client while muster types: the drive stops, and nothing is submitted.
func TestTmuxAKeystrokeMidDriveStopsIt(t *testing.T) {
	qs := []askq.Question{{Question: "Which pets do you have?", Header: "Pets", MultiSelect: true, Options: []askq.Option{{Label: "Cat"}, {Label: "Dog"}, {Label: "Fish"}}}}
	r, b, pane, result, rw := typingRig(t, "keystroke", qs)
	done := make(chan error, 1)
	go func() { done <- b.AnswerAsk(context.Background(), pane, qs, []askq.Choice{{Picks: []int{0, 2}}}) }()
	time.Sleep(600 * time.Millisecond)
	if _, err := rw.Write([]byte("2")); err != nil {
		t.Fatal(err)
	}
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the drive never finished")
	}
	if !errors.Is(err, askdrive.ErrLost) && !errors.Is(err, askdrive.ErrNotUntouched) {
		t.Fatalf("a drive with a keystroke in it: %v\n%s", err, r.pane("keystroke"))
	}
	if !strings.Contains(r.pane("keystroke"), "[✔] Dog") {
		t.Fatalf("the control: the operator's keystroke did not reach the picker:\n%s", r.pane("keystroke"))
	}
	time.Sleep(500 * time.Millisecond)
	if res, ok := readResult(t, result); ok {
		t.Fatalf("a picker someone typed into was submitted: %v", res)
	}
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
	if has, known := b.RawPaste(context.Background()); !has || !known {
		t.Fatalf("this tmux (>= %s) reports paste-buffer -S has=%v known=%v", MinRawPasteTmux, has, known)
	}
	if err := b.AnswerAsk(context.Background(), pane, qs, []askq.Choice{{Picks: []int{1}}}); err != nil {
		t.Fatal(err)
	}
	got := r.run("list-buffers", "-F", "#{buffer_name}")
	if strings.Contains(got, askBufferPrefix+"0123456789ab") || !strings.Contains(got, "operators-own") || !strings.Contains(got, live) {
		t.Fatalf("buffers after a drive: %q", got)
	}
}
