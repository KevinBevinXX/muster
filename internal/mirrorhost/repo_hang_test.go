package mirrorhost

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestAHungFilesystemDoesNotFreezeTheRepoLookup (#151): a stat that never
// returns (a dead NFS/sshfs mount) costs the listing path at most the walk's
// timeout, after which the window is named by its cwd; the next reads start NO
// second walk of that cwd while the first is stuck, and answer at once; and
// when the stuck stat finally returns, its answer replaces the fallback.
func TestAHungFilesystemDoesNotFreezeTheRepoLookup(t *testing.T) {
	root := repoTree(t)
	cwd := filepath.Join(root, "src/fixture-repo/cmd/tool")
	release := make(chan struct{})
	var hung atomic.Int32
	r := &repoNamer{timeout: 200 * time.Millisecond, stat: func(p string) (os.FileInfo, error) {
		if strings.HasSuffix(p, filepath.Join("cmd", ".git")) {
			hung.Add(1)
			<-release
		}
		return os.Stat(p)
	}}
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })

	start := time.Now()
	if got := r.name(cwd); got != "tool" {
		t.Fatalf("a hung walk named the window %q, want its cwd's own name %q", got, "tool")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the hung lookup took %s", took)
	}
	// Past the cache TTL too, a stuck walk is not started again.
	r.now = func() time.Time { return time.Now().Add(2 * repoCacheTTL) }
	for i := 0; i < 5; i++ {
		start = time.Now()
		if got := r.name(cwd); got != "tool" {
			t.Fatalf("read %d while stuck: %q", i, got)
		}
		if took := time.Since(start); took > 100*time.Millisecond {
			t.Fatalf("read %d while stuck waited %s: a second walk was started", i, took)
		}
	}
	if n := hung.Load(); n != 1 {
		t.Fatalf("%d walks reached the hung stat, want 1", n)
	}
	r.now = nil
	once.Do(func() { close(release) })
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		r.mu.Lock()
		e := r.cache[cwd]
		r.mu.Unlock()
		if e.name == "fixture-repo" {
			return
		}
	}
	t.Fatal("the walk's late answer never replaced the fallback")
}

// devInfo is a FileInfo whose stat result names device dev.
type devInfo struct {
	os.FileInfo
	dev uint64
}

func (d devInfo) Sys() any { return &syscall.Stat_t{Dev: d.dev} }

// TestTheRepoWalkStopsAtAFilesystemBoundary (#151): a cwd on a filesystem
// mounted inside a repository is not in that repository — git's own discovery
// stops at the mount — so the walk does not climb out of it to the repository's
// .git, and the window is named by its cwd.
func TestTheRepoWalkStopsAtAFilesystemBoundary(t *testing.T) {
	root := repoTree(t)
	mount := filepath.Join(root, "src/fixture-repo/mnt")
	cwd := filepath.Join(mount, "inner")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := func(p string) (os.FileInfo, error) {
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		dev := uint64(41)
		if p == mount || strings.HasPrefix(p, mount+string(filepath.Separator)) {
			dev = 42 // the mounted filesystem
		}
		return devInfo{fi, dev}, nil
	}
	if got := repoName(cwd, stat); got != "inner" {
		t.Fatalf("a cwd on a mount inside a repository was named %q, want %q", got, "inner")
	}
	// The same tree on ONE filesystem walks up to the repository, as before.
	same := func(p string) (os.FileInfo, error) {
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		return devInfo{fi, 41}, nil
	}
	if got := repoName(cwd, same); got != "fixture-repo" {
		t.Fatalf("on one filesystem: %q, want fixture-repo", got)
	}
}
