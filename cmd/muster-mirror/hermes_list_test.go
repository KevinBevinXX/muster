package main

import (
	"context"
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
		"s\t@3\t0\tactive\tzsh\n"
	if out != want {
		t.Fatalf("-list -windows printed\n%q\nwant\n%q", out, want)
	}
}

// -list -hermes names the hook's consent mismatch and, per window, why it has
// no rendered view or an unknown status.
func TestListHermesDiagnoses(t *testing.T) {
	withBackend(t, hermesListBackend{})
	home := t.TempDir()
	cmd := "/nix/store/x-muster-mirror/bin/muster-mirror hermes-hook"
	// Only some pairs allowlisted, and one for an older binary.
	old := "/nix/store/old-muster-mirror/bin/muster-mirror hermes-hook"
	if _, err := hermeshost.NewHome(home).WriteAllowlist(old, time.Now()); err != nil {
		t.Fatal(err)
	}
	rc, out, errs := runWith(t, map[string]string{envHermesHome: home}, "-list", "-hermes", "-hermes-hook-command", cmd)
	if rc != 0 {
		t.Fatalf("rc %d %s", rc, errs)
	}
	for _, want := range []string{
		"hook-command\t" + cmd + "\n",
		"hook-consent\tMISSING\tpre_llm_call,on_human_input_request,on_human_input_resolved,on_session_end,on_session_finalize\t",
		"hook-other\tpre_llm_call " + old + "\t",
		"window\th\t@1\tpane=%1\tcli=41\ttty=/dev/pts/4\tsession=" + listSID + "\tstatus=idle\n",
		"window\th\t@2\tpane=%2\tcli=42\ttty=/dev/pts/5\tsession=-\tstatus=unknown\n",
		"  lineage\t1 session(s)\ttip=local-old\tended:resumed_other\n",
		"  no-view\t" + hermeshost.NoViewSwitched + "\n",
		"hermes-windows\t2\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("-list -hermes lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "window\ts\t") {
		t.Fatal("a shell window was diagnosed as Hermes")
	}
	// Once the pairs are written, consent is ok.
	if _, err := hermeshost.NewHome(home).WriteAllowlist(cmd, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, out, _ = runWith(t, map[string]string{envHermesHome: home}, "-list", "-hermes", "-hermes-hook-command", cmd)
	if !strings.Contains(out, "hook-consent\tok\t") || strings.Contains(out, "hook-other") {
		t.Fatalf("after the writer:\n%s", out)
	}
}
