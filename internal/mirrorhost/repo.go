package mirrorhost

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE REPO OF A WINDOW (muster's repo chip, mirror/repos.go): the git top-level
// directory's name for the window's Claude Code working directory, else that
// directory's own name.
//
// 🔴 NO git PROCESS, AND NOT EVEN A stat PER WINDOW PER TICK. The top level is
// found the way git itself finds it — the nearest directory, from the cwd up,
// holding a `.git` (a directory, or a worktree's `.git` file) — with one stat
// per level, and the answer is CACHED PER CWD (repoNamer) for repoCacheTTL: a
// host whose windows sit in the same directories costs nothing between
// expiries, however many windows and ticks. TestTheRepoLookupIsCachedPerCwd
// counts the stats.
//
// 🔴 A HUNG FILESYSTEM MUST NOT FREEZE THE WINDOW LIST (#151). A stat on a dead
// NFS or sshfs mount blocks in the kernel, and this lookup runs in the listing
// path. So a walk gets repoWalkTimeout; past it the window is named by its cwd
// alone (cached like any answer), and the walk — which cannot be cancelled —
// is left to finish in the background, ONE per cwd at a time, its answer
// replacing the fallback if it ever returns.
//
// 🔴 THE WALK STOPS AT A FILESYSTEM BOUNDARY, as git's own discovery does
// (without GIT_DISCOVERY_ACROSS_FILESYSTEM): a cwd on a mount inside a
// repository is not in that repository, and walking up out of a mount is how
// a lookup reaches a filesystem it was never asked about.

// repoCacheTTL is how long a cwd's answer is kept: a `git init` (or a deleted
// .git) shows within this long.
const repoCacheTTL = 5 * time.Minute

// repoCacheMax bounds the cache; past it, it is emptied and refilled.
const repoCacheMax = 1024

// repoWalkMax bounds the walk up from a cwd.
const repoWalkMax = 64

// repoWalkTimeout bounds one cwd's walk in the listing path.
const repoWalkTimeout = 300 * time.Millisecond

// deletedSuffix is what Linux appends to a /proc/<pid>/cwd link whose directory
// was removed.
const deletedSuffix = " (deleted)"

// repoNamer answers repoName per cwd, cached.
type repoNamer struct {
	// stat is os.Stat; a test counts its calls.
	stat func(string) (os.FileInfo, error)
	// now is time.Now; a test moves it.
	now func() time.Time
	// timeout is repoWalkTimeout when 0; a test shortens it.
	timeout time.Duration

	mu    sync.Mutex
	cache map[string]repoEntry
	// walking is the cwds whose walk outlived its timeout and still runs.
	walking map[string]bool
}

type repoEntry struct {
	name string
	at   time.Time
}

// name is cwd's repo name (repoName), from the cache while it is fresh.
func (r *repoNamer) name(cwd string) string {
	if cwd == "" {
		return ""
	}
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	t := now()
	r.mu.Lock()
	if e, ok := r.cache[cwd]; ok && t.Sub(e.at) < repoCacheTTL {
		r.mu.Unlock()
		return e.name
	}
	if r.walking[cwd] {
		// A walk of this cwd is still stuck: start no second one.
		r.mu.Unlock()
		return cwdName(cwd)
	}
	r.mu.Unlock()
	stat := os.Stat
	if r.stat != nil {
		stat = r.stat
	}
	timeout := r.timeout
	if timeout <= 0 {
		timeout = repoWalkTimeout
	}
	done := make(chan string, 1)
	go func() { done <- repoName(cwd, stat) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case n := <-done:
		r.store(cwd, n, t)
		return n
	case <-timer.C:
	}
	n := cwdName(cwd)
	r.mu.Lock()
	if r.walking == nil {
		r.walking = map[string]bool{}
	}
	r.walking[cwd] = true
	r.mu.Unlock()
	r.store(cwd, n, t)
	go func() {
		late := <-done
		r.mu.Lock()
		delete(r.walking, cwd)
		r.mu.Unlock()
		r.store(cwd, late, now())
	}()
	return n
}

func (r *repoNamer) store(cwd, n string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil || len(r.cache) >= repoCacheMax {
		r.cache = map[string]repoEntry{}
	}
	r.cache[cwd] = repoEntry{name: n, at: at}
}

// cwdName is the name for cwd itself, with no filesystem access: what
// repoName answers when no .git is found.
func cwdName(cwd string) string {
	cwd = strings.TrimSuffix(cwd, deletedSuffix)
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	start := filepath.Clean(cwd)
	if start == filepath.Dir(start) {
		return ""
	}
	return mirror.CleanRepoName(filepath.Base(start))
}

// deviceOf is the filesystem a stat result is on; false when the platform's
// stat carries none.
func deviceOf(fi os.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, false
	}
	return uint64(st.Dev), true //nolint:unconvert // int32 on darwin
}

// repoName is the name muster shows for a window working in cwd: the basename
// of the nearest directory from cwd up holding a `.git` (git's own top level,
// a worktree's included), else cwd's own basename; "" for no cwd, a relative
// one, or the filesystem root. A cwd whose directory was deleted (Linux's
// " (deleted)" link) is looked up by its path without the suffix: a deleted
// subdirectory of a repository still names the repository, and a deleted
// directory outside one is named by itself. The name is CleanRepoName's.
//
// The walk does not leave cwd's filesystem: a parent on another device ends it
// (git's discovery boundary). A cwd that cannot be stat'ed (deleted) has no
// device to keep to, and walks as before.
func repoName(cwd string, stat func(string) (os.FileInfo, error)) string {
	cwd = strings.TrimSuffix(cwd, deletedSuffix)
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	start := filepath.Clean(cwd)
	var dev uint64
	keepDev := false
	if fi, err := stat(start); err == nil {
		dev, keepDev = deviceOf(fi)
	}
	d := start
	for i := 0; i < repoWalkMax; i++ {
		if _, err := stat(filepath.Join(d, ".git")); err == nil {
			if d == filepath.Dir(d) {
				break // a .git at the root names nothing
			}
			return mirror.CleanRepoName(filepath.Base(d))
		}
		if d == filepath.Dir(d) {
			break
		}
		parent := filepath.Dir(d)
		if keepDev {
			fi, err := stat(parent)
			if err != nil {
				break
			}
			if pd, ok := deviceOf(fi); ok && pd != dev {
				break // a filesystem boundary: git stops here too
			}
		}
		d = parent
	}
	return cwdName(start)
}

// windowRepos is EVERY window's repo name, by window id — a shell's as much as
// a Claude Code session's: what a v2 per-window host sends as its repos
// message.
func (r *repoNamer) windowRepos(windows []WindowInfo) map[string]string {
	var out map[string]string
	for _, w := range windows {
		if w.Cwd == "" {
			continue
		}
		if n := r.name(w.Cwd); n != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[w.ID] = n
		}
	}
	return out
}
