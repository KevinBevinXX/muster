package hermeshost

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
)

// THE SECRET-PATH LEDGER (docs/design/hermes.md, "Secret hygiene"): every path
// under a Hermes home muster-mirror opens goes through home.go, and the set is
// ReadPaths exactly.

// TestTheReadAllowlistIsExactly pins the allowlist itself: a path added or
// removed is a design change, made here on purpose.
func TestTheReadAllowlistIsExactly(t *testing.T) {
	want := []string{"state.db", "state.db-wal", "state.db-shm", "terminal-sessions/tty-*", "shell-hooks-allowlist.json"}
	if !reflect.DeepEqual(ReadPaths, want) {
		t.Fatalf("ReadPaths = %q\nwant %q — a change here is a change to docs/design/hermes.md's Secret hygiene", ReadPaths, want)
	}
	for _, p := range ReadPaths {
		for _, never := range []string{"auth.json", ".env", "config.yaml", "pastes", "active_sessions", "memories", "logs"} {
			if strings.Contains(p, never) {
				t.Errorf("ReadPaths holds %q", p)
			}
		}
	}
}

// TestOnlyHomeGoReadsTheHermesHome is the structural half: Home.dir — the only
// way to name a path under the home — is read in home.go and nowhere else in
// the package. A new file building a Hermes path of its own fails here.
func TestOnlyHomeGoReadsTheHermesHome(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		ast.Inspect(af, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "dir" && f != "home.go" {
				t.Errorf("%s reads .dir (the Hermes home) outside home.go", fset.Position(sel.Pos()))
			}
			return true
		})
	}
	if checked < 5 {
		t.Fatalf("parsed only %d files: the glob is wrong", checked)
	}
}

// inotifyOpens watches every directory under root for IN_OPEN and returns a
// function that stops and reports every path (relative to root) opened since:
// a FILE, never a directory listing.
func inotifyOpens(t *testing.T, root string) func() []string {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatalf("inotify (the ledger cannot watch, so it fails rather than skips): %v", err)
	}
	wd := map[int32]string{}
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		w, err := unix.InotifyAddWatch(fd, p, unix.IN_OPEN)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		wd[int32(w)] = rel
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return func() []string {
		defer unix.Close(fd)
		seen := map[string]bool{}
		buf := make([]byte, 64<<10)
		for {
			n, err := unix.Read(fd, buf)
			if n <= 0 || err != nil {
				break
			}
			for off := 0; off+unix.SizeofInotifyEvent <= n; {
				ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
				name := ""
				if ev.Len > 0 {
					raw := buf[off+unix.SizeofInotifyEvent : off+unix.SizeofInotifyEvent+int(ev.Len)]
					name = strings.TrimRight(string(raw), "\x00")
				}
				if ev.Mask&unix.IN_ISDIR == 0 && name != "" {
					seen[filepath.Join(wd[ev.Wd], name)] = true
				}
				off += unix.SizeofInotifyEvent + int(ev.Len)
			}
		}
		var out []string
		for p := range seen {
			out = append(out, p)
		}
		sort.Strings(out)
		return out
	}
}

// allowed reports whether rel matches a ReadPaths glob.
func allowed(rel string) bool {
	for _, g := range ReadPaths {
		if ok, _ := filepath.Match(g, rel); ok {
			return true
		}
	}
	return false
}

// exerciseReadPath runs everything the daemon reads a Hermes home for. Every
// reader added to the daemon is added HERE, so the ledger sees it.
var exerciseReadPath = []func(t *testing.T, h *hermestest.Home, root string){
	func(t *testing.T, h *hermestest.Home, root string) {
		st := NewHome(h.Dir).OpenState()
		defer st.Close()
		loc := &Locator{State: st, Status: StatusDir(t.TempDir()), Alive: func(int, string) bool { return true }}
		if ps := loc.Locate(ctx(t), tty, time.Now().Add(-time.Hour)); ps.Session != root {
			t.Fatalf("the exercise did not resolve: %+v", ps)
		}
	},
	func(t *testing.T, h *hermestest.Home, root string) {
		if _, err := NewHome(h.Dir).CheckAllowlist(testCommand); err != nil {
			t.Fatal(err)
		}
	},
}

// TestTheHermesHomeLedger is the behavioural half: the whole read path runs
// against a home planted with every secret-bearing file, under inotify, and
// every file opened must be on ReadPaths. The positive control: state.db, the
// breadcrumb and the allowlist ARE seen opened, so a zero for the decoys is
// the watch working, not the watch blind.
func TestTheHermesHomeLedger(t *testing.T) {
	h := hermestest.New(t)
	h.Decoy()
	root := hermestest.LocalID()
	h.Session(root, hermestest.SessionOpts{})
	h.Message(root, hermestest.Msg{Role: "user", Content: "hi"})
	h.Breadcrumb(tty, root, hermestest.Now())
	if _, err := NewHome(h.Dir).WriteAllowlist(testCommand, time.Now()); err != nil {
		t.Fatal(err)
	}
	stop := inotifyOpens(t, h.Dir)
	for _, ex := range exerciseReadPath {
		ex(t, h, root)
	}
	opened := stop()
	for _, p := range opened {
		if !allowed(p) {
			t.Errorf("the read path opened %s, which is not on ReadPaths", p)
		}
	}
	for _, must := range []string{"state.db", "terminal-sessions/tty-dev-pts-7", "shell-hooks-allowlist.json"} {
		found := false
		for _, p := range opened {
			found = found || p == must
		}
		if !found {
			t.Errorf("positive control: %s was not seen opened (opened: %q) — the watch is blind", must, opened)
		}
	}
}

// The ledger catches a reader that strays: reading a decoy through the same
// watch is reported.
func TestTheLedgerSeesAStrayRead(t *testing.T) {
	h := hermestest.New(t)
	h.Decoy()
	stop := inotifyOpens(t, h.Dir)
	b, err := os.ReadFile(filepath.Join(h.Dir, "auth.json"))
	if err != nil || len(b) == 0 {
		t.Fatal(err)
	}
	opened := stop()
	if !reflect.DeepEqual(opened, []string{"auth.json"}) {
		t.Fatalf("opened = %q, want [auth.json]", opened)
	}
	if allowed("auth.json") || allowed("pastes/paste-1.txt") || allowed("runtime/active_sessions.json") || allowed(".env") {
		t.Fatal("a secret path matches ReadPaths")
	}
}
