package mirrorhost

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// repoCacheTTL is how long a cwd's answer is kept: a `git init` (or a deleted
// .git) shows within this long.
const repoCacheTTL = 5 * time.Minute

// repoCacheMax bounds the cache; past it, it is emptied and refilled.
const repoCacheMax = 1024

// repoWalkMax bounds the walk up from a cwd.
const repoWalkMax = 64

// deletedSuffix is what Linux appends to a /proc/<pid>/cwd link whose directory
// was removed.
const deletedSuffix = " (deleted)"

// repoNamer answers repoName per cwd, cached.
type repoNamer struct {
	// stat is os.Stat; a test counts its calls.
	stat func(string) (os.FileInfo, error)
	// now is time.Now; a test moves it.
	now func() time.Time

	mu    sync.Mutex
	cache map[string]repoEntry
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
	r.mu.Unlock()
	stat := os.Stat
	if r.stat != nil {
		stat = r.stat
	}
	n := repoName(cwd, stat)
	r.mu.Lock()
	if r.cache == nil || len(r.cache) >= repoCacheMax {
		r.cache = map[string]repoEntry{}
	}
	r.cache[cwd] = repoEntry{name: n, at: t}
	r.mu.Unlock()
	return n
}

// repoName is the name muster shows for a window working in cwd: the basename
// of the nearest directory from cwd up holding a `.git` (git's own top level,
// a worktree's included), else cwd's own basename; "" for no cwd, a relative
// one, or the filesystem root. A cwd whose directory was deleted (Linux's
// " (deleted)" link) is looked up by its path without the suffix: a deleted
// subdirectory of a repository still names the repository, and a deleted
// directory outside one is named by itself. The name is CleanRepoName's.
func repoName(cwd string, stat func(string) (os.FileInfo, error)) string {
	cwd = strings.TrimSuffix(cwd, deletedSuffix)
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	start := filepath.Clean(cwd)
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
		d = filepath.Dir(d)
	}
	if start == filepath.Dir(start) {
		return ""
	}
	return mirror.CleanRepoName(filepath.Base(start))
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
