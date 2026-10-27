package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
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
		!strings.Contains(err.Error(), "could not be replaced") ||
		!strings.Contains(err.Error(), unwritableRecovery(parent)) {
		t.Fatalf("seed error = %v, want one naming %s as unwritable and not replaceable, and how to recover", err, parent)
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
// writable workspace that already exists — the agent's, ccd-created on its
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

// A SYMLINK on the path is never replaced, whatever it points at. os.Remove on
// a symlink deletes the link, not its target, so the rmdir-only guarantee ("a
// non-empty directory is never replaced") does not hold through one: before
// #162, a workspace symlinked to a non-empty unwritable directory was
// unlinked, logged as "an empty directory", and recreated empty — the target's
// files no longer at the workspace path. A link to an EMPTY unwritable
// directory is refused too: the link is the operator's, not a directory this
// process could have made.
//
// Both places the loop can reach a symlink: the workspace itself (an agent
// without a repository) and the workspace's deepest existing ancestor (with
// one: CCD_WORKSPACE=/data/workspace/<repo>, the link at /data/workspace).
func TestSeedRefusesToReplaceASymlinkToAnUnwritableDirectory(t *testing.T) {
	for _, tc := range []struct {
		name, under string
		empty       bool
	}{
		{"workspace itself", "", false},
		{"ancestor of the workspace", "app", false},
		{"workspace itself, empty target", "", true},
		{"ancestor of the workspace, empty target", "app", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			volume, target := volumeWithUnwritableDir(t, "target")
			if !tc.empty {
				if err := os.Chmod(target, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(target, "keep"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(target, 0o555); err != nil {
					t.Fatal(err)
				}
			}
			link := filepath.Join(volume, "workspace")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			resolved, err := filepath.EvalSymlinks(target)
			if err != nil {
				t.Fatal(err)
			}
			err, log := seedErr(filepath.Join(volume, "claude"), filepath.Join(link, tc.under))
			if err == nil || !strings.Contains(err.Error(), link+" is a symlink to a directory this process (uid") ||
				!strings.Contains(err.Error(), "mode d") || !strings.Contains(err.Error(), "a symlink is never replaced") ||
				!strings.Contains(err.Error(), unwritableRecovery(resolved)) {
				t.Fatalf("seed error = %v, want one naming %s as a symlink, with its mode, and the fix at its target %s", err, link, resolved)
			}
			if strings.Contains(log, "replaced") {
				t.Fatalf("seed logged a replacement: %q", log)
			}
			if got, lerr := os.Readlink(link); lerr != nil || got != target {
				t.Fatalf("the symlink was changed: %q %v, want -> %s", got, lerr, target)
			}
			entries, rerr := os.ReadDir(target)
			if rerr != nil || (tc.empty && len(entries) != 0) {
				t.Fatalf("the target was touched: %v %v", entries, rerr)
			}
			if !tc.empty {
				if b, rerr := os.ReadFile(filepath.Join(target, "keep")); rerr != nil || string(b) != "x" {
					t.Fatalf("the target's file was touched: %q %v", b, rerr)
				}
			}
		})
	}
}

// 🔴 THE REMOVAL ITSELF NEVER DELETES ANYTHING BUT AN EMPTY DIRECTORY, whatever
// is at the path by the time it runs — ensureWritableDir's Lstat refuses a
// symlink first, so only this reaches the primitive with one (what a symlink
// swapped in after that check would meet). os.Remove would unlink the link and
// the file (unlink(2) before rmdir(2)).
func TestRemoveEmptyDirRemovesOnlyAnEmptyDirectory(t *testing.T) {
	d := t.TempDir()
	empty, full, file, link := filepath.Join(d, "empty"), filepath.Join(d, "full"), filepath.Join(d, "file"), filepath.Join(d, "link")
	for _, dir := range []string{empty, full} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(full, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(empty, link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{link, file, full} {
		if err := removeEmptyDir(p); err == nil {
			t.Errorf("removeEmptyDir(%s) succeeded; want it refused", filepath.Base(p))
		}
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("REMOVED: %s is gone after removeEmptyDir: %v", filepath.Base(p), err)
		}
	}
	// Control: an empty directory is removed.
	if err := removeEmptyDir(empty); err != nil {
		t.Fatalf("control: removeEmptyDir(empty) = %v", err)
	}
	if _, err := os.Lstat(empty); !os.IsNotExist(err) {
		t.Fatalf("control: the empty directory is still there: %v", err)
	}
}

// Invariant guard (not regression coverage — the bug never touched it): a
// symlink to a WRITABLE directory is used through, as before; the refusal is only
// for a symlink that would otherwise have been replaced.
func TestSeedUsesASymlinkToAWritableDirectory(t *testing.T) {
	volume := t.TempDir()
	target := filepath.Join(volume, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(volume, "workspace")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	err, log := seedErr(filepath.Join(volume, "claude"), filepath.Join(link, "app"))
	if err != nil || strings.Contains(log, "replaced") {
		t.Fatalf("seed through a writable symlink: %v %q", err, log)
	}
	if fi, serr := os.Stat(filepath.Join(target, "app")); serr != nil || !fi.IsDir() {
		t.Fatalf("the checkout dir was not created through the link: %v %v", fi, serr)
	}
}

// A refusal must say how to recover: the pod never starts, so `kubectl exec` into
// it is impossible. The fix runs from a debug pod in the agent's namespace and is
// ONE command whoever owns the directory: `chown -R` (only root can make a
// root-owned one this uid's) then `chmod u+rwx` (an owner without write).
func TestUnwritableRecoveryNamesADebugPodAndTheCommand(t *testing.T) {
	got := unwritableRecovery("/data/workspace")
	want := "The agent's pod cannot start until it is fixed, so `kubectl exec` into it is impossible: " +
		"from a debug pod in the agent's namespace (`<MUSTER_AGENT_NAMESPACE_PREFIX><agent>`, e.g. " +
		"`muster-agent-<agent>`) that mounts the agent's volume (PVC <agent>-workspace) at /data, run " +
		"`chown -R " + strconv.Itoa(os.Getuid()) + " /data/workspace && chmod u+rwx /data/workspace` as root " +
		"(see docs/provisioner.md, \"A workspace ccd cannot write in\")"
	if got != want {
		t.Errorf("unwritableRecovery:\n got %q\nwant %q", got, want)
	}
}
