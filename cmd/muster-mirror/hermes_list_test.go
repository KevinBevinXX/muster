package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

const listSID = "local-" + "cd" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"

// hermesListBackend lists one Hermes window with a session and one whose
// breadcrumb is stale, each as the locator resolved it.
type hermesListBackend struct{ listBackend }

func (hermesListBackend) Windows(context.Context) ([]mirrorhost.WindowInfo, error) {
	return []mirrorhost.WindowInfo{
		{Session: "h", ID: "@1", Index: 0, Active: true, Pane: "%1", TTY: "/dev/pts/4", Command: "python3.14",
			Agent: &mirrorhost.AgentPane{Runtime: mirror.RuntimeHermes, Pane: "%1", PID: 41, Gen: listSID, Status: mirror.StatusIdle,
				StatusSince: 1, Source: mirrorhost.SourceBreadcrumb, Hermes: &hermeshost.PaneSession{
					Breadcrumb: hermeshost.Breadcrumb{SessionID: listSID, TS: 1700000000},
					Lineage:    hermeshost.Lineage{{ID: listSID}}, Session: listSID,
					Status: hermeshost.Believed{Status: mirror.StatusIdle, Since: 1}}}},
		{Session: "h", ID: "@2", Index: 1, Pane: "%2", TTY: "/dev/pts/5", Command: "python3.14",
			Agent: &mirrorhost.AgentPane{Runtime: mirror.RuntimeHermes, Pane: "%2", PID: 42, Source: mirrorhost.SourceProcess,
				Hermes: &hermeshost.PaneSession{
					Breadcrumb: hermeshost.Breadcrumb{SessionID: "local-old", TS: 1700000000},
					Lineage:    hermeshost.Lineage{{ID: "local-old", Ended: true, EndReason: "resumed_other"}},
					NoView:     hermeshost.NoViewSwitched, Status: hermeshost.Believed{Reason: hermeshost.NoViewSwitched}}}},
		{Session: "h", ID: "@4", Index: 2, Pane: "%4", TTY: "/dev/pts/6", Command: "python3.14",
			Agent: &mirrorhost.AgentPane{Runtime: mirror.RuntimeHermes, Pane: "%4", PID: 44, Gen: listSID, Source: mirrorhost.SourceBreadcrumb,
				Hermes: &hermeshost.PaneSession{
					Breadcrumb: hermeshost.Breadcrumb{SessionID: listSID, TS: 1700000000},
					Lineage:    hermeshost.Lineage{{ID: listSID}}, Session: listSID,
					Status: hermeshost.Believed{Reason: hermeshost.ReasonNoStatusFile}}}},
		{Session: "s", ID: "@3", Index: 0, Active: true, Pane: "%3", Command: "zsh"},
	}, nil
}

func withBackend(t *testing.T, b mirrorhost.Backend) {
	prev := e2eBackend
	e2eBackend = func() mirrorhost.Backend { return b }
	t.Cleanup(func() { e2eBackend = prev })
}

// -list -windows prints a Hermes window as `hermes <pane> <state> <session
// id or -> <source>`.
func TestListWindowsPrintsAHermesWindowsSession(t *testing.T) {
	withBackend(t, hermesListBackend{})
	rc, out, errs := runWith(t, nil, "-list", "-windows")
	if rc != 0 {
		t.Fatalf("rc %d %s", rc, errs)
	}
	want := "h\t@1\t0\tactive\tpython3.14\thermes\t%1\tidle\t" + listSID + "\tbreadcrumb\n" +
		"h\t@2\t1\t-\tpython3.14\thermes\t%2\tunknown\t-\tprocess\n" +
		"h\t@4\t2\t-\tpython3.14\thermes\t%4\tunknown\t" + listSID + "\tbreadcrumb\n" +
		"s\t@3\t0\tactive\tzsh\n"
	if out != want {
		t.Fatalf("-list -windows printed\n%q\nwant\n%q", out, want)
	}
}

const (
	binX = "/nix/store/x-muster-mirror/bin/muster-mirror hermes-hook"
	binY = "/nix/store/y-muster-mirror/bin/muster-mirror hermes-hook"
)

// hermesConfig writes a Hermes config.yaml into home declaring the hook for
// every event with command, plus keys that must never be read (an inline key).
func hermesConfig(t *testing.T, home, command string, events ...string) {
	t.Helper()
	if len(events) == 0 {
		events = hermeshost.HookEvents
	}
	var b strings.Builder
	b.WriteString("model:\n  api_key: sk-or-v1-INLINE-SECRET-0000\nhooks:\n")
	for _, ev := range events {
		b.WriteString("  " + ev + ":\n    - command: \"" + command + "\"\n      timeout: 5\n")
	}
	b.WriteString("  pre_tool_call:\n    - command: \"/h/guard.sh\"\n")
	must(t, os.WriteFile(filepath.Join(home, "config.yaml"), []byte(b.String()), 0o600))
}

func listHermes(t *testing.T, home string, extra map[string]string) string {
	t.Helper()
	env := map[string]string{envHermesHome: home, envHermesStatusDir: t.TempDir()}
	for k, v := range extra {
		env[k] = v
	}
	rc, out, errs := runWith(t, env, "-list", "-hermes")
	if rc != 0 {
		t.Fatalf("rc %d %s", rc, errs)
	}
	if strings.Contains(out, "INLINE-SECRET") || strings.Contains(errs, "INLINE-SECRET") {
		t.Fatal("-list -hermes printed config.yaml content beyond its hooks")
	}
	return out
}

// 🔴 THE AUDITOR's TWO-BINARY REPRO (round-1 audit, #191 🟡1). Hermes's config
// declares binary X, and the allowlist holds X's five pairs: consent is OK for
// the command Hermes ACTUALLY runs — even though this muster-mirror is binary
// Y. The old diagnostic checked Y, said MISSING, and suggested
// `hermes-allowlist -command Y`, which would have REMOVED X's consent (the
// writer drops other muster-mirror entries).
func TestListHermesChecksTheDeclaredCommandNotThisBinary(t *testing.T) {
	withBackend(t, hermesListBackend{})
	home := t.TempDir()
	hermesConfig(t, home, binX)
	if _, err := hermeshost.NewHome(home).WriteAllowlist(binX, time.Now()); err != nil {
		t.Fatal(err)
	}
	out := listHermes(t, home, nil)
	if !strings.Contains(out, "hook-consent\tok for "+binX+"\n") {
		t.Fatalf("not ok for the declared command:\n%s", out)
	}
	if strings.Contains(out, "MISSING") || strings.Contains(out, "hermes-allowlist") {
		t.Fatalf("a fix was suggested though consent holds:\n%s", out)
	}
	for _, ev := range hermeshost.HookEvents {
		if !strings.Contains(out, "hook-declared\t"+ev+"\t"+binX+"\n") {
			t.Fatalf("the declared %s hook is not listed:\n%s", ev, out)
		}
	}
}

// A missing pair is reported for the DECLARED command, and the fix names the
// declared command and keeps -hermes-home.
func TestListHermesFixTargetsTheDeclaredCommand(t *testing.T) {
	withBackend(t, hermesListBackend{})
	home := t.TempDir()
	hermesConfig(t, home, binX)
	// Only Y is allowlisted: X's hooks are skipped by the gateway.
	if _, err := hermeshost.NewHome(home).WriteAllowlist(binY, time.Now()); err != nil {
		t.Fatal(err)
	}
	out := listHermes(t, home, nil)
	want := "hook-consent\tMISSING for " + binX + "\t" + strings.Join(hermeshost.HookEvents, ",") + "\t"
	if !strings.Contains(out, want) {
		t.Fatalf("lacks %q:\n%s", want, out)
	}
	fix := "muster-mirror hermes-allowlist -hermes-home \"" + home + "\" -command \"" + binX + "\""
	if !strings.Contains(out, fix) {
		t.Fatalf("the fix is not %q:\n%s", fix, out)
	}
	if strings.Contains(out, "-command \""+binY) {
		t.Fatalf("a fix names a command Hermes does not declare:\n%s", out)
	}
	if !strings.Contains(out, "hook-other\tpre_llm_call "+binY+"\t") {
		t.Fatalf("the undeclared allowlisted entries are not named:\n%s", out)
	}
}

// Two muster-mirror commands declared: the writer keeps only one, so no fix is
// printed for either (it would remove the other's consent).
func TestListHermesPrintsNoFixForTwoDeclaredCommands(t *testing.T) {
	withBackend(t, hermesListBackend{})
	home := t.TempDir()
	var b strings.Builder
	b.WriteString("hooks:\n  pre_llm_call:\n    - command: \"" + binX + "\"\n    - command: \"" + binY + "\"\n")
	must(t, os.WriteFile(filepath.Join(home, "config.yaml"), []byte(b.String()), 0o600))
	out := listHermes(t, home, nil)
	if strings.Contains(out, "hermes-allowlist") || !strings.Contains(out, "hook-config\tseveral") {
		t.Fatalf("two declared commands:\n%s", out)
	}
}

// No hook declared: said so, and no allowlist fix (the allowlist is not the
// problem).
func TestListHermesWithNoDeclaredHook(t *testing.T) {
	withBackend(t, hermesListBackend{})
	home := t.TempDir()
	must(t, os.WriteFile(filepath.Join(home, "config.yaml"), []byte("model:\n  default: x\n"), 0o600))
	out := listHermes(t, home, nil)
	if !strings.Contains(out, "hook-config\tnone") || strings.Contains(out, "hermes-allowlist") {
		t.Fatalf("no declared hook:\n%s", out)
	}
}

// Per window: the breadcrumb line, the status file the lineage resolves to,
// the no-view reason and the unknown reason (round-1 audit, #191 🟡2).
func TestListHermesPrintsEachWindowsResolution(t *testing.T) {
	withBackend(t, hermesListBackend{})
	home := t.TempDir()
	hermesConfig(t, home, binX)
	status := t.TempDir()
	must(t, hermeshost.StatusDir(status).Write(hermeshost.StatusFile{SessionID: listSID, Status: mirror.StatusIdle,
		Since: 1700000000000, WriterPID: 50, WriterStart: "1"}))
	out := listHermes(t, home, map[string]string{envHermesStatusDir: status})
	for _, want := range []string{
		"window\th\t@1\tpane=%1\tcli=41\ttty=/dev/pts/4\tsession=" + listSID + "\tstatus=idle\n",
		"  breadcrumb\t" + listSID + "\tts=2023-11-14T22:13:20Z\n",
		"  status-file\t" + listSID + "\tidle\tsince=2023-11-14T22:13:20Z\twriter=50\n",
		"window\th\t@2\tpane=%2\tcli=42\ttty=/dev/pts/5\tsession=-\tstatus=unknown\n",
		"  lineage\t1 session(s)\ttip=local-old\tended:resumed_other\n",
		"  no-view\t" + hermeshost.NoViewSwitched + "\n",
		"window\th\t@4\tpane=%4\tcli=44\ttty=/dev/pts/6\tsession=" + listSID + "\tstatus=unknown\n",
		"  unknown\t" + hermeshost.ReasonNoStatusFile + "\n",
		"hermes-windows\t3\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("-list -hermes lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "window\ts\t") {
		t.Fatal("a shell window was diagnosed as Hermes")
	}
}

// The unknown-status hint names a flag this binary has (round-1 audit, #189
// 🟡4): `muster-mirror -list -hermes`.
func TestTheNoStatusHintNamesARealFlag(t *testing.T) {
	if !strings.Contains(hermeshost.ReasonNoStatusFile, "muster-mirror -list -hermes") {
		t.Fatalf("hint = %q", hermeshost.ReasonNoStatusFile)
	}
	withBackend(t, hermesListBackend{})
	if rc, _, errs := runWith(t, map[string]string{envHermesHome: t.TempDir()}, "-list", "-hermes"); rc != 0 {
		t.Fatalf("the flag the hint names fails: rc %d %s", rc, errs)
	}
}
