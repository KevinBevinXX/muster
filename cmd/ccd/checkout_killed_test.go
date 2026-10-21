package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAKilledCloneIsNeverACheckout: a clone killed mid-transfer (its bound
// fired — the same SIGKILL ccd's shutdown or an eviction delivers) leaves no
// .git in the workspace, so it is not ready; the next attempt, unhindered,
// clones. The transfer is held open by a pack-objects hook that sleeps, so the
// kill lands after git has created its repository — the state an in-place
// clone would have left behind as a .git with no commit.
func TestAKilledCloneIsNeverACheckout(t *testing.T) {
	root := t.TempDir()
	gh := newFakeGitHub(t, root)
	for _, e := range gh.env() {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	gh.publish(t, "example-org/tide-charts", map[string]string{"README.md": "r\n"})
	plain, err := os.ReadFile(gh.gitconfig)
	if err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(root, "slow-pack")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nsleep 3\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gh.gitconfig, append(plain, []byte("[uploadpack]\n\tpackObjectsHook = "+hook+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "ws", "tide-charts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	c := newTestCheckout(t, "example-org/tide-charts", "", dir, "")
	c.Timeout = 700 * time.Millisecond
	if err := c.Attempt(context.Background()); err == nil {
		t.Fatal("a killed clone reported success")
	}
	if err := c.Attempt(context.Background()); err == nil {
		t.Fatal("the attempt after a killed clone took what it left for a checkout")
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		t.Fatal("a killed clone left a .git in the workspace")
	}
	// Unhindered, the next attempt clones.
	if err := os.WriteFile(gh.gitconfig, plain, 0o644); err != nil {
		t.Fatal(err)
	}
	c.Timeout = 0
	if err := c.Attempt(context.Background()); err != nil {
		t.Fatalf("after the kill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Fatal("the checkout has no README.md")
	}
}

// TestAGitDirectoryWithNoCommitIsRefused: a workspace holding a .git whose HEAD
// names no commit (what a clone killed in place leaves) is not a checkout and
// is neither cloned over nor removed.
func TestAGitDirectoryWithNoCommitIsRefused(t *testing.T) {
	root := t.TempDir()
	gh := newFakeGitHub(t, root)
	for _, e := range gh.env() {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	gh.publish(t, "example-org/tide-charts", map[string]string{"README.md": "r\n"})
	dir := filepath.Join(root, "ws", "tide-charts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, nil, dir, "init", "-q")
	c := newTestCheckout(t, "example-org/tide-charts", "", dir, "")
	err := c.Attempt(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no commit checked out") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err != nil {
		t.Fatal("the .git was removed")
	}
}
