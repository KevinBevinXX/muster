package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestTheWorkdirHeaderIsMusters pins the spelling ccd reads to the one muster
// sends.
func TestTheWorkdirHeaderIsMusters(t *testing.T) {
	if headerCCWorkdir != agents.HeaderCCWorkdir {
		t.Fatalf("ccd reads %q, muster sends %q", headerCCWorkdir, agents.HeaderCCWorkdir)
	}
	if headerCCWorkdirUsed != agents.HeaderCCWorkdirUsed {
		t.Fatalf("ccd answers %q, muster reads %q", headerCCWorkdirUsed, agents.HeaderCCWorkdirUsed)
	}
}

// TestCheckJobDirTakesOnlyAnExistingJobWorktreeOfItsWorkspace: everything that
// is not exactly <workspace's parent>/.muster-jobs/<id>, an existing directory,
// is refused with the failure that says why; the one that is, is accepted —
// and the CLI's config is not written either way.
func TestCheckJobDirTakesOnlyAnExistingJobWorktreeOfItsWorkspace(t *testing.T) {
	root := t.TempDir()
	ws, cfg := filepath.Join(root, "workspace", "widgets"), filepath.Join(root, "claude")
	seedInto(t, cfg, ws)
	before, err := os.ReadFile(filepath.Join(cfg, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	jobs := filepath.Join(root, "workspace", ".muster-jobs")
	ok := filepath.Join(jobs, "17")
	if err := os.MkdirAll(ok, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobs, "18"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ws, filepath.Join(jobs, "19")); err != nil {
		t.Fatal(err)
	}
	l := &lanes{configDir: cfg, workspace: ws}
	for _, tc := range []struct {
		dir, typ string
	}{
		{ws, failInvalidInput},
		{"/etc", failInvalidInput},
		{jobs, failInvalidInput},
		{filepath.Join(jobs, "abc"), failInvalidInput},
		{filepath.Join(jobs, "0"), failInvalidInput},
		{filepath.Join(jobs, "17", "sub"), failInvalidInput},
		{filepath.Join(jobs, "17") + "/", failInvalidInput},
		{jobs + "/../.muster-jobs/17", failInvalidInput},
		{"workspace/.muster-jobs/17", failInvalidInput},
		{filepath.Join(jobs, "18"), failInvalidInput}, // a file
		{filepath.Join(jobs, "19"), failInvalidInput}, // a symlink to the checkout
		{filepath.Join(jobs, "20"), failNotReady},     // not there (yet)
	} {
		if f := l.checkJobDir(tc.dir); f == nil || f.Type != tc.typ {
			t.Errorf("%q: %v, want a %s failure", tc.dir, f, tc.typ)
		}
	}
	if f := l.checkJobDir(ok); f != nil {
		t.Fatalf("%q: %v, want accepted", ok, f)
	}
	if after, _ := os.ReadFile(filepath.Join(cfg, ".claude.json")); !bytes.Equal(after, before) {
		t.Fatalf(".claude.json changed:\n%s\nwas\n%s", after, before)
	}
}
