package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 🔴 NO LANE STARTS BEFORE THE REPOSITORY IS CHECKED OUT. main.go holds the
// primary back until the checkout exists (checkout.wait); a lane must wait too.
// A lane CLI started in the empty seeded workspace answers without the
// repository's CLAUDE.md and .claude/, and any file it writes there makes every
// later clone refuse the directory ("holds files but is not a git checkout"), so
// the primary never starts. (The removed lane-before-primary gate held this
// back as a side effect: the primary cannot send SessionStart before the
// checkout exists.) Control: once the checkout is ready, the lane starts.
func TestNoLaneStartsBeforeTheCheckoutExists(t *testing.T) {
	for _, state := range []string{checkoutCloning, checkoutAwaitingGrant, checkoutFailed} {
		t.Run(state, func(t *testing.T) {
			cfg := t.TempDir()
			primary, lanePane := newKillablePane(), newKillablePane()
			s := newServer(serverConfig{Bearer: "b", ConfigDir: cfg, SubmitTimeout: 200 * time.Millisecond}, primary, newAuthTracker())
			sup := newSupervisor(primary, "claude", cfg, "/data/workspace")
			fastRestarts(sup)
			s.supervisePrimary(sup, noPrimaryPID)
			co := &checkout{repo: "o/r", dir: "/data/workspace", done: make(chan struct{}),
				st: checkoutState{Repo: "o/r", Dir: "/data/workspace", State: state, Detail: "detail"}}
			s.checkout = co
			l := testLanes(t, 2)
			l.startTimeout = 300 * time.Millisecond
			l.newTerm = func(string) laneTerminal { return lanePane }
			l.tweak = fastRestarts
			s.runLanes(l)
			starts := func() int {
				lanePane.mu.Lock()
				defer lanePane.mu.Unlock()
				return lanePane.starts
			}

			code, body := turnStatus(t, s, "sess-a")
			time.Sleep(100 * time.Millisecond) // a lane CLI the turn started would be counted by now
			if ln := l.get(laneID("sess-a")); ln != nil {
				ln.stop()
			}
			if starts() != 0 || code != http.StatusServiceUnavailable || !strings.Contains(body, failNotReady) ||
				!strings.Contains(body, "the Claude Code session has not started") {
				t.Fatalf("A LANE BEFORE THE CHECKOUT (%s): %d lane start(s), status %d, body %s; "+
					"want 503 not_ready naming the checkout and no lane started", state, starts(), code, body)
			}

			// Control: the checkout exists; the lane starts.
			co.mu.Lock()
			co.st.State = checkoutReady
			co.mu.Unlock()
			if ln, f := s.lanes.acquire(context.Background(), "sess-a", ""); f == nil {
				t.Cleanup(ln.stop)
				s.lanes.release(ln)
			}
			waitUntil(t, "control: the lane's CLI started once the checkout is ready", func() bool { return starts() == 1 })
		})
	}
}
