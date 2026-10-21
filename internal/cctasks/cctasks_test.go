package cctasks

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// fakeTree is a /proc fixture: each process's fd links, and the process
// table's child lists and argv the caller would pass.
type fakeTree struct {
	t        *testing.T
	root     string
	children map[int][]int
	argv     map[int][]string
}

func newTree(t *testing.T) *fakeTree {
	return &fakeTree{t: t, root: t.TempDir(), children: map[int][]int{}, argv: map[int][]string{}}
}

// add is pid, a child of ppid, with fds 0, 1 and 2 linked to fd0..fd2 (""
// = no link) and argv.
func (f *fakeTree) add(pid, ppid int, fd0, fd1, fd2 string, argv ...string) {
	f.t.Helper()
	dir := filepath.Join(f.root, fmt.Sprint(pid), "fd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	for i, l := range []string{fd0, fd1, fd2} {
		if l != "" {
			if err := os.Symlink(l, filepath.Join(dir, fmt.Sprint(i))); err != nil {
				f.t.Fatal(err)
			}
		}
	}
	f.children[ppid] = append(f.children[ppid], pid)
	sort.Ints(f.children[ppid])
	f.argv[pid] = argv
}

func (f *fakeTree) count(cli int, config string) Shells {
	return CountShells(f.root, cli, f.children, func(p int) []string { return f.argv[p] }, config)
}

const (
	cfg    = "/fx/config"
	tasks  = "/fx/tmp/claude-1000/-fx-repo/0f0f0f0f-sess/tasks/"
	sock   = "socket:[41001]"
	snap   = "/fx/config/shell-snapshots/snapshot-zsh-1791903000-abcdef.sh"
	devnul = "/dev/null"
	pipeIn = "pipe:[41002]"
)

// TestCountShellsReadsTheCLIsProcessTree: distinct Bash task ids among the
// CLI's LIVE descendants, by fd 1 or fd 2. Every count is distinct (3, then 2
// after a removal) so a mutant that hardcodes or double-counts cannot pass.
func TestCountShellsReadsTheCLIsProcessTree(t *testing.T) {
	f := newTree(t)
	const cli = 100
	// A background shell: the wrapper's stdout on its task file.
	f.add(110, cli, devnul, tasks+"b0000000a.output", tasks+"b0000000a.output", "/bin/zsh", "-c", "source "+snap+" && sleep 60")
	// Its child: the same task id again — counted once.
	f.add(111, 110, devnul, tasks+"b0000000a.output", tasks+"b0000000a.output", "sleep", "60")
	// A subagent's shell: a grandchild, its task file on fd 2 only.
	f.add(120, cli, sock, sock, sock, "node", "subagent-runner")
	f.add(121, 120, sock, "pipe:[9]", tasks+"b00000z9q.output", "make", "test")
	// A third task.
	f.add(130, cli, sock, tasks+"bzz00000c.output", tasks+"bzz00000c.output", "/bin/zsh", "-c", "go test")
	// NOT counted: a subagent's output symlink name (a<16 hex>), a malformed
	// id, a file outside tasks/, and an orphan reparented to init.
	f.add(140, cli, devnul, tasks+"a0123456789abcdef.output", "", "x")
	f.add(141, cli, devnul, tasks+"b000.output", "", "x")
	f.add(142, cli, devnul, "/fx/elsewhere/b0000000d.output", "", "x")
	f.add(150, 1, devnul, tasks+"b0000000e.output", tasks+"b0000000e.output", "nohup", "daemon")

	if got := f.count(cli, cfg); got != (Shells{N: 3}) {
		t.Fatalf("CountShells = %+v, want 3 shells and no Monitor", got)
	}
	// Negative control: the same tree, the third task gone.
	f.children[cli] = []int{110, 120, 140, 141, 142}
	if got := f.count(cli, cfg); got != (Shells{N: 2}) {
		t.Fatalf("after the task ended, CountShells = %+v, want 2", got)
	}
	// Positive control for the orphan: counted from ITS parent, it is a shell.
	if got := f.count(1, cfg); got.N != 1 {
		t.Fatalf("instrument check: the orphan's own task, counted from init, = %+v, want 1", got)
	}
}

// TestAMonitorIsSeenOnlyInItsRealLayout: a DIRECT child of the CLI whose argv
// names the shell snapshot, with stdin /dev/null and stdout and stderr sockets.
// Its negative controls — an MCP server's shape (sockets, no snapshot argv),
// a snapshot-argv GRANDchild, and one whose stdin is not /dev/null — are not.
func TestAMonitorIsSeenOnlyInItsRealLayout(t *testing.T) {
	cases := []struct {
		name  string
		build func(f *fakeTree)
		want  Shells
	}{
		{"a Monitor alone", func(f *fakeTree) {
			f.add(210, 200, devnul, sock, sock, "/bin/zsh", "-c", "source "+snap+" && tail -f log")
		}, Shells{Monitor: true}},
		{"a Monitor beside one task shell", func(f *fakeTree) {
			f.add(210, 200, devnul, sock, sock, "/bin/zsh", "-c", "source "+snap+" && tail -f log")
			f.add(220, 200, devnul, tasks+"b0000000a.output", tasks+"b0000000a.output", "/bin/zsh", "-c", "source "+snap+" && sleep 9")
		}, Shells{N: 1, Monitor: true}},
		{"an MCP server's shape is not one", func(f *fakeTree) {
			f.add(210, 200, devnul, sock, sock, "node", "/fx/mcp-server.js")
		}, Shells{}},
		{"a snapshot-argv grandchild is not one", func(f *fakeTree) {
			f.add(205, 200, sock, sock, sock, "node", "subagent-runner")
			f.add(210, 205, devnul, sock, sock, "/bin/zsh", "-c", "source "+snap+" && tail -f log")
		}, Shells{}},
		{"a socket stdin is not one", func(f *fakeTree) {
			f.add(210, 200, pipeIn, sock, sock, "/bin/zsh", "-c", "source "+snap+" && tail -f log")
		}, Shells{}},
		{"another config dir's snapshot is not one", func(f *fakeTree) {
			f.add(210, 200, devnul, sock, sock, "/bin/zsh", "-c", "source /fx/other/shell-snapshots/snapshot-zsh-1.sh")
		}, Shells{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTree(t)
			c.build(f)
			if got := f.count(200, cfg); got != c.want {
				t.Fatalf("CountShells = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestDecideIsTheWorkRule: every status crossed with the process count and
// the ledger. Every count is distinct (3 subagents, 2 shells, …) so a mutant
// that swaps the fields or hardcodes one cannot pass.
func TestDecideIsTheWorkRule(t *testing.T) {
	asked := 0
	ledger := func(n int) func() int { return func() int { asked++; return n } }
	cases := []struct {
		name    string
		status  string
		sh      Shells
		running int
		want    Work
		ok      bool
		asks    bool
	}{
		// idle: the CLI says nothing runs. A stale ledger entry and leftover
		// processes are both overruled — and the ledger is not even read.
		{"idle overrules a stale ledger and stray processes", StatusIdle, Shells{N: 2}, 3, Work{}, true, false},
		// shell: no subagents; the count is exact without a Monitor.
		{"shell with 3 task shells is exactly 3", StatusShell, Shells{N: 3}, 4, Work{Shells: 3}, true, false},
		{"shell with none seen is 1+, never 0", StatusShell, Shells{}, 4, Work{Shells: 1, ShellsAtLeast: true}, true, false},
		{"shell with a Monitor alone is 1+", StatusShell, Shells{Monitor: true}, 4, Work{Shells: 1, ShellsAtLeast: true}, true, false},
		{"shell with a Monitor beside 2 is 2+, not 2", StatusShell, Shells{N: 2, Monitor: true}, 4, Work{Shells: 2, ShellsAtLeast: true}, true, false},
		// busy / waiting: a foreground call may be among them, so 1+.
		{"busy with 3 shells is 1+, not 3", StatusBusy, Shells{N: 3}, 0, Work{Shells: 1, ShellsAtLeast: true}, true, true},
		{"busy (background agents only) with 2 shells is 1+ beside the ledger's 1", StatusBusy, Shells{N: 2}, 1, Work{Subagents: 1, Shells: 1, ShellsAtLeast: true}, true, true},
		{"busy with no shells is the ledger's count alone", StatusBusy, Shells{}, 3, Work{Subagents: 3}, true, true},
		{"waiting with a Monitor is 1+ beside the ledger's 2", StatusWaiting, Shells{Monitor: true}, 2, Work{Subagents: 2, Shells: 1, ShellsAtLeast: true}, true, true},
		// No status (no live sessions file), or one this side does not know.
		{"no status: no Work at all", "", Shells{N: 3}, 2, Work{}, false, false},
		{"an unknown status: no Work at all", "compacting", Shells{N: 3}, 2, Work{}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			asked = 0
			got, ok := Decide(c.status, c.sh, ledger(c.running))
			if got != c.want || ok != c.ok {
				t.Fatalf("Decide(%q, %+v, %d) = %+v, %v; want %+v, %v", c.status, c.sh, c.running, got, ok, c.want, c.ok)
			}
			if (asked > 0) != c.asks {
				t.Fatalf("the transcript ledger was read %d times; want read=%v (an idle window costs no transcript read)", asked, c.asks)
			}
		})
	}
}
