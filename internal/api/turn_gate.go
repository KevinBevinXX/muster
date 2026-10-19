package api

import (
	"errors"
	"sync"
)

// turnGate counts the chat turns in flight per agent (runUserTurn: the chat
// socket, the POST send, a route hand-back) and lets the chief replacement CLOSE
// an agent to new turns, atomically with checking that none is running.
//
// 🔴 WHY THIS AND NOT THE SESSION CLAIM. A plugin delivery, a kickoff and a memo
// turn hold the chief's session claim (agents.Store.ClaimKickoff); an operator's
// chat turn does not, and making it take one would change chat (a message typed
// during a delivery would be refused). The replacement needs only "no chat turn
// is running, and none starts until I am done", which this answers in-process.
//
// ⚠ IN-PROCESS ONLY. muster runs one replica by design (its deployment says why);
// a second replica's chat turns would not be seen here.
type turnGate struct {
	mu      sync.Mutex
	running map[int64]int
	closed  map[int64]bool
}

// ErrAgentBeingReplaced refuses a chat turn to an agent the chief replacement has
// closed: it is about to be deleted.
var ErrAgentBeingReplaced = errors.New("this agent is being replaced; send the message to the new chief once it is up")

// enter starts a turn for id; false (nothing counted) when id is closed.
func (g *turnGate) enter(id int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed[id] {
		return false
	}
	if g.running == nil {
		g.running = map[int64]int{}
	}
	g.running[id]++
	return true
}

// enterIdle starts a turn for id only when no other turn of id's is running
// here: the answer delivery's door (attention_delivery.go), which must never
// overlap the agent's own turn — an operator's chat turn takes no session
// claim, so the claim alone would not see it. ok false (nothing counted) with
// why: "closed" (being replaced) or "busy" (a turn is running).
func (g *turnGate) enterIdle(id int64) (ok bool, why string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed[id] {
		return false, "closed"
	}
	if g.running[id] > 0 {
		return false, "busy"
	}
	if g.running == nil {
		g.running = map[int64]int{}
	}
	g.running[id]++
	return true, ""
}

// leave ends a turn enter counted.
func (g *turnGate) leave(id int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running[id] <= 1 {
		delete(g.running, id)
		return
	}
	g.running[id]--
}

// close stops new turns for id, reporting false (and closing nothing) when a
// turn is running. reopen undoes it.
func (g *turnGate) close(id int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running[id] > 0 {
		return false
	}
	if g.closed == nil {
		g.closed = map[int64]bool{}
	}
	g.closed[id] = true
	return true
}

func (g *turnGate) reopen(id int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.closed, id)
}
