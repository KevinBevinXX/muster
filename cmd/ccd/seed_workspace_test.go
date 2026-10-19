package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The workspace a container runtime pre-created as ROOT on a fresh volume
// (seed.go ensureWritableDir): /data/workspace, root-owned 0755, while ccd runs
// as uid 1000. Simulated here by a directory this test owns chmod'ed so that the
// test's own uid cannot create files in it — the permission check that fails is
// the same one (no write bit for the caller), only the reason differs.

// volumeWithUnwritableDir is a writable "volume" holding name as an empty
// directory the caller cannot create files in (0555), restored on cleanup so
// t.TempDir can remove it.
func volumeWithUnwritableDir(t *testing.T, name string) (volume, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so an unwritable directory cannot be simulated")
	}
	volume = t.TempDir()
	dir = filepath.Join(volume, name)
	if err := os.Mkdir(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	// Control: the simulation really is unwritable for this process.
	if err := os.Mkdir(filepath.Join(dir, "probe"), 0o700); !os.IsPermission(err) {
		t.Fatalf("control: mkdir inside the 0555 dir returned %v, want permission denied", err)
	}
	return volume, dir
}

func seedErr(cfg, ws string) (error, string) {
	var stderr bytes.Buffer
	err := seed(seedOpts{configDir: cfg, workspace: ws, settingsTemplate: imageSettingsTemplate}, &stderr)
	return err, stderr.String()
}

// The live 0.49.0 failure: an agent with a repository has
// CCD_WORKSPACE=/data/workspace/<repo>, and /data/workspace is the runtime's
// root-owned 0755 directory. Before the fix: `mkdir /data/workspace/app:
// permission denied`.
func TestSeedCreatesARepoCheckoutUnderAnUnwritableEmptyWorkspace(t *testing.T) {
	volume, parent := volumeWithUnwritableDir(t, "workspace")
	ws := filepath.Join(parent, "app")
	err, log := seedErr(filepath.Join(volume, "claude"), ws)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, d := range []string{parent, ws} {
		if err := os.WriteFile(filepath.Join(d, "probe"), nil, 0o600); err != nil {
			t.Fatalf("%s is not writable after seed: %v", d, err)
		}
	}
	if !strings.Contains(log, "ccd seed: replaced "+parent+": it was an empty directory") {
		t.Fatalf("seed did not say it replaced %s: %q", parent, log)
	}
}

// The same root-owned /data/workspace on an agent WITHOUT a repository: the CLI's
// own working directory, which it could not write a file in.
func TestSeedReplacesAnUnwritableEmptyWorkspaceItself(t *testing.T) {
	volume, ws := volumeWithUnwritableDir(t, "workspace")
	err, log := seedErr(filepath.Join(volume, "claude"), ws)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	fi, err := os.Stat(ws)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("workspace after seed: %v %v, want a 0700 directory", fi, err)
	}
	if err := os.WriteFile(filepath.Join(ws, "probe"), nil, 0o600); err != nil {
		t.Fatalf("workspace not writable after seed: %v", err)
	}
	if !strings.Contains(log, "ccd seed: replaced "+ws) {
		t.Fatalf("seed did not say it replaced %s: %q", ws, log)
	}
}

// Never delete content: an unwritable directory that holds anything is an
// error naming it, and is left exactly as it was — including a WRITABLE
// subdirectory's files, which a recursive removal could reach (an unwritable
// directory protects only its own entries).
func TestSeedRefusesToReplaceAnUnwritableDirectoryThatHoldsFiles(t *testing.T) {
	volume, parent := volumeWithUnwritableDir(t, "workspace")
	other := filepath.Join(parent, "other")
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	err, _ := seedErr(filepath.Join(volume, "claude"), filepath.Join(parent, "app"))
	if err == nil || !strings.Contains(err.Error(), parent+" is a directory this process") ||
		!strings.Contains(err.Error(), "could not be replaced") {
		t.Fatalf("seed error = %v, want one naming %s as unwritable and not replaceable", err, parent)
	}
	if b, rerr := os.ReadFile(filepath.Join(other, "keep")); rerr != nil || string(b) != "x" {
		t.Fatalf("a file under the directory was touched: %q %v", b, rerr)
	}
}

// A directory this process could not have removed itself (its parent is not
// writable either) is not removed.
func TestSeedDoesNotReplaceADirectoryWhoseParentIsUnwritable(t *testing.T) {
	volume, parent := volumeWithUnwritableDir(t, "workspace")
	if err := os.Chmod(volume, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(volume, 0o755) })
	cfg := t.TempDir()
	err, _ := seedErr(cfg, filepath.Join(parent, "app"))
	if err == nil || !strings.Contains(err.Error(), "could not be replaced") {
		t.Fatalf("seed error = %v, want could not be replaced", err)
	}
	if fi, serr := os.Stat(parent); serr != nil || fi.Mode().Perm() != 0o555 {
		t.Fatalf("the directory was changed: %v %v", fi, serr)
	}
}

// Invariant guard (not regression coverage — the bug never touched it): a
// writable workspace that already exists — the chief's, ccd-created on its
// volume — is used as it is: its files kept, nothing replaced, nothing logged.
func TestSeedLeavesAWritableWorkspaceAlone(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(ws, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err, log := seedErr(t.TempDir(), ws)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(log, "replaced") {
		t.Fatalf("seed replaced something in a writable workspace: %q", log)
	}
	fi, _ := os.Stat(ws)
	if b, _ := os.ReadFile(filepath.Join(ws, "keep")); string(b) != "x" || fi.Mode().Perm() != 0o750 {
		t.Fatalf("the workspace changed: mode %v, file %q", fi.Mode(), b)
	}
}
