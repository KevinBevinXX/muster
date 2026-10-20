package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/podagent"
)

// 🔴 NO LANE STARTS BEFORE THE REPOSITORY IS CHECKED OUT. main.go holds the
// primary back until the checkout exists (checkout.wait); a lane must wait too.
// A lane CLI started in the empty seeded workspace answers without the
// repository's CLAUDE.md and .claude/, and any file it writes there makes every
// later clone refuse the directory ("holds files but is not a git checkout"), so
// the primary never starts. (The removed lane-before-primary gate held this
// back as a side effect: the primary cannot send SessionStart before the
// checkout exists.) Control: once the checkout is ready, a turn through the
// same path starts the lane and is served.
func TestNoLaneStartsBeforeTheCheckoutExists(t *testing.T) {
	for _, state := range []string{checkoutCloning, checkoutAwaitingGrant, checkoutFailed} {
		t.Run(state, func(t *testing.T) {
			cfg := t.TempDir()
			primary, lanePane := newKillablePane(), newKillablePane()
			s := newServer(serverConfig{Bearer: "b", ConfigDir: cfg, SubmitTimeout: 200 * time.Millisecond}, primary, newAuthTracker())
			sup := newSupervisor(primary, "claude", cfg, "/data/workspace")
			fastRestarts(sup)
			s.supervisePrimary(sup, noPrimaryPID)
			co := podagent.NewCheckout(podagent.CheckoutConfig{Repo: "o/r", Dir: "/data/workspace", Voice: ccdVoice})
			co.SetStateForTest(state, "detail")
			s.checkout = co
			l := testLanes(t, 2)
			l.startTimeout = 300 * time.Millisecond
			l.newTerm = func(string) laneTerminal { return lanePane }
			l.newServer = func(term terminal) *server {
				return newServer(serverConfig{Bearer: "b", ConfigDir: cfg, SubmitTimeout: 200 * time.Millisecond}, term, newAuthTracker())
			}
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

			// Control, through the REAL turn path (laneFor), not acquire: once the
			// checkout exists, the lane starts and the turn is served — so a
			// check that refused lanes for good would fail here.
			co.SetStateForTest(checkoutReady, "")
			l.startTimeout = 5 * time.Second
			type res struct {
				code int
				body string
			}
			done := make(chan res, 1)
			go func() { code, body := turnStatus(t, s, "sess-a"); done <- res{code, body} }()
			var ln *lane
			for deadline := time.Now().Add(5 * time.Second); ln == nil && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
				select {
				case got := <-done:
					t.Fatalf("LANES REFUSED AFTER THE CHECKOUT (%s): status %d, %d lane start(s), body %s; "+
						"want the lane started and the turn pasted into it", state, got.code, starts(), got.body)
				default:
				}
				ln = l.get(laneID("sess-a"))
			}
			if ln == nil {
				t.Fatal("control: the lane was never started once the checkout was ready")
			}
			t.Cleanup(ln.stop)
			waitUntil(t, "control: the lane's CLI started once the checkout is ready", func() bool { return starts() == 1 })
			ln.srv.onHook(hookEvent{Event: "SessionStart", SessionID: laneUUID("sess-a")})
			if got := <-done; got.code != http.StatusGatewayTimeout || lanePane.pasted() != 1 {
				t.Fatalf("control (%s): a turn after the checkout: status %d, %d paste(s) into the lane, body %s; "+
					"want the turn pasted into the lane (504 unsubmitted here)", state, got.code, lanePane.pasted(), got.body)
			}
		})
	}
}
