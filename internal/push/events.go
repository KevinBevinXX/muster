package push

// The notification events a user can receive, each with a toggle on /account.
//
// 🔴 EVERY ONE IS ON BY DEFAULT (operator decision): push_preferences stores
// what a user MUTED, so a user who never opened the page gets everything. That
// is also why a per-owner rate cap and muster_push_delivery_total ship with
// it rather than later — "everything on" is the amplification risk.
const (
	EventReview    = "review"    // a task enters ready_for_review
	EventResolved  = "resolved"  // a task leaves review: clears its notification
	EventConnector = "connector" // an agent requests a connector
	EventAgent     = "agent"     // a task's agent is up and working
	EventCreated   = "created"   // a machine (producer) created tasks
	EventComment   = "comment"   // an external (machine) comment on a task
	EventPlugin    = "plugin"    // a plugin reply draft awaits its owner's approval
)

// Event is one toggle.
type Event struct {
	Name  string
	Label string
}

// Events is the closed vocabulary, in the order /account shows it.
var Events = []Event{
	{EventReview, "A task is ready for review"},
	{EventResolved, "Clear a task's notification when it leaves review"},
	{EventConnector, "An agent requests a connector"},
	{EventAgent, "A task's agent is up and working"},
	{EventCreated, "A machine created tasks"},
	{EventComment, "A machine commented on a task"},
	{EventPlugin, "A reply draft awaits your approval"},
}

// KnownEvent reports whether name is in Events.
func KnownEvent(name string) bool {
	for _, e := range Events {
		if e.Name == name {
			return true
		}
	}
	return false
}

// TypeResolved is the payload type the service worker reads as "close the
// notification with this tag and show nothing".
const TypeResolved = "resolved"

// applePushHost is Safari's web push service.
const applePushHost = "web.push.apple.com"
