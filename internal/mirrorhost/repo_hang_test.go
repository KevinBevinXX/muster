package mirrorhost

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
