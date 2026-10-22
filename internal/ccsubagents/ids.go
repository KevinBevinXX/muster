// Package ccsubagents is the ONE reading of a Claude Code session's subagents:
// which Agent calls a parent transcript made, what each subagent's own files
// say, the status rule that decides running / done / failed (State), and the
// work ledger that counts how many run now (Ledger).
//
// 🔴 ONE RULE, ONE PLACE, TWO READERS. muster-mirror reads it for a mirrored
// window (internal/mirrorhost: the rendered view's subagent cards and the
// card's live work chip) and ccd reads it inside a dispatched claude-code pod
// (cmd/ccd/work.go), so the two kinds of card cannot count subagents
// differently.
//
// 🔴 A LEAF: it imports the standard library, internal/cctranscript and
// internal/redact only. It is linked into the agent pod's ccd, so it must not
// bring the wire package (internal/mirror, which pulls a websocket client) or
// the host package (internal/mirrorhost, which pulls object-store and database
// clients) with it. The wire's subagent names and bounds live HERE, and
// internal/mirror re-exports them unchanged; internal/modulegate's
// TestTheSubagentLeafPullsNoNetworkOrStorageClient pins the dependency set.
package ccsubagents

import "regexp"

// MaxSubagents bounds one view's summary list (internal/mirror re-exports it
// as mirror.MaxSubagents, a property of the wire).
const MaxSubagents = 64

// BackfillWindow is the tail of a transcript file a fresh reader starts from
// (mirror.BackfillWindow): the file is never read wholesale.
const BackfillWindow = 4 << 20

// The values of a subagent's state (mirror.SubagentRunning and so on).
const (
	Running = "running"
	Done    = "done"
	Failed  = "failed"
)

// tools are the tool names whose calls spawn a subagent: "Agent" (Claude Code
// 2.1), and "Task", its earlier name.
var tools = map[string]bool{"Agent": true, "Task": true}

// IsSubagentTool reports whether a tool call of this name spawns a subagent
// (mirror.IsSubagentTool).
func IsSubagentTool(name string) bool { return tools[name] }

// idPattern is a subagent id or a tool_use id as the wire carries one: the
// CLI's are hex (a3f…) and toolu_… — bounded so a summary's worst case is
// known, and safe as one path element on the host.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidID reports whether id is a subagent (or tool_use) id the wire carries
// (mirror.ValidSubagentID).
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Step is one step of a subagent: a tool call or a reply's text
// (mirror.SubagentStep is this type).
type Step struct {
	// Tool is the tool a call step ran; "" for a text step.
	Tool string `json:"tool,omitempty"`
	// Text is the call's one-line target, or the text's first line. Redacted.
	Text string `json:"text"`
	At   int64  `json:"at,omitempty"`
}
