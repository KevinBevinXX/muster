//go:build tmuxit

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/ZacxDev/muster/internal/agentgateway"
)

// 🔴 TestTmuxTheCLIHoldsNoTurnCredential: the whole binary, real tmux, the CLI
// started exactly as ccd starts it. ccd is given every turn credential it can be
// given — HOOKS_TOKEN, MUSTER_GATEWAY_BEARER (agreeing, as muster ships them) and
// the gateway runtime's own CCD_GATEWAY_UPSTREAM_BEARER — and then:
//
//  1. the CLI's environment, and the environment of a process the CLI spawns (a
//     tool call's view), hold none of them, by name or by value;
//  2. ccd's own /proc/<pid>/environ, which still holds the block ccd was exec'd
//     with, cannot be read by a process of ccd's user (this test is one);
//  3. the hook path still authenticates: the CLI's SessionStart (sent by the
//     real `ccd hook` with the credential the CLI DOES keep) makes ccd ready,
//     and a turn round-trips with muster's bearer.
//
// The positive controls: CCD_HOOK_SECRET IS in both dumps (the dumps can see a
// variable ccd hands the CLI), and the fake CLI's own /proc environ IS readable
// (the read can succeed where nothing hides it).
func TestTmuxTheCLIHoldsNoTurnCredential(t *testing.T) {
	const upstreamBearer = "fixture-retired-upstream-credential-3e9d"
	derived := agentgateway.HooksSHA256().Bearer(knownHooksToken)
	r := startServe(t, "MUSTER_GATEWAY_BEARER="+derived, envGatewayUpstreamBearer+"="+upstreamBearer)
	r.waitHealth(t, "primary start", primaryReady)

	// 3. A turn, with muster's bearer, end to end.
	reply, err := mustersGatewayNoT(r.gwAddr).Chat(context.Background(), contractAgent, "s", "hello", nil)
	if err != nil || reply == "" {
		t.Fatalf("a turn with muster's bearer: %q, %v\nccd log:\n%s", reply, err, r.logs.String())
	}

	dumps, _ := filepath.Glob(filepath.Join(r.cfg, "fake-*env.*"))
	var cli, child int
	for _, d := range dumps {
		b, err := os.ReadFile(d)
		if err != nil {
			t.Fatal(err)
		}
		env := string(b)
		switch {
		case strings.Contains(filepath.Base(d), "child"):
			child++
		default:
			cli++
		}
		if !strings.Contains(env, envHookSecret+"=") || !strings.Contains(env, envHookSocket+"=") {
			t.Fatalf("control: %s lacks the hook channel ccd hands the CLI; the dump sees nothing:\n%s", filepath.Base(d), env)
		}
		for _, name := range turnCredentialEnv {
			if strings.Contains(env, name+"=") {
				t.Errorf("%s: the CLI's process tree holds %s", filepath.Base(d), name)
			}
		}
		for what, v := range map[string]string{"the hooks token": knownHooksToken, "ccd's bearer": derived,
			"the retired gateway runtime's credential": upstreamBearer} {
			if strings.Contains(env, v) {
				t.Errorf("%s: the CLI's process tree holds %s's value", filepath.Base(d), what)
			}
		}
	}
	if cli == 0 || child == 0 {
		t.Fatalf("instrument check: %d CLI and %d child environment dumps (want >=1 each): %v", cli, child, dumps)
	}

	// 2. ccd's /proc environ: refused to its own user.
	_, err = os.ReadFile(filepath.Join("/proc", strconv.Itoa(r.pid), "environ"))
	if !errors.Is(err, syscall.EACCES) && !errors.Is(err, os.ErrPermission) {
		t.Errorf("reading ccd's /proc/%d/environ as ccd's user: %v; want permission denied", r.pid, err)
	}
	// Control: the CLI's own environ (dumpable) IS readable by the same user.
	var cliPid string
	for _, d := range dumps {
		if b := filepath.Base(d); strings.HasPrefix(b, "fake-env.") {
			cliPid = strings.TrimPrefix(b, "fake-env.")
		}
	}
	b, err := os.ReadFile(filepath.Join("/proc", cliPid, "environ"))
	if err != nil || !strings.Contains(string(b), envHookSecret+"=") {
		t.Errorf("control: the fake CLI's /proc/%s/environ (dumpable, same user): %v; want it readable, with the "+
			"hook channel in it", cliPid, err)
	}
}
