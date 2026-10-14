package mirror

import (
	"slices"
	"sort"
	"strconv"
)

// SessionView is one mirrored entry as a LIST or a page shows it — a WINDOW of
// a per-window host, or a whole session of a per-session host — with its host's
// facts beside its own. HostView.Entries and HostView.Find are the only
// producers.
type SessionView struct {
	Host    string
	Session string
	// Window is the tmux window id (Window.ID) the entry is; "" = the entry is
	// a per-session host's session, addressed by name alone.
	Window string
	// Index is the window's index (display only); meaningful when Window != "".
	Index int
	// Active: the session's current window (always true for a per-session
	// host's entry, which IS the session's active window).
	Active bool
	// Label is the host's display name for the entry, "" for none.
	Label string
	// Command is what runs in the window's active pane, "" for unknown.
	Command string
	// Activity is when the entry was last active on its host (Unix seconds,
	// floored to ActivityBucket); 0 = the host did not say.
	Activity int64
	// Online, Interactive, Rendered, PanePaste: the host's (HostView). Claude:
	// the host reports Claude Code in this entry's pane.
	Online      bool
	Interactive bool
	Rendered    bool
	PanePaste   bool
	Claude      bool
}

// Title is the entry's display name: the host's label when it sent one, else
// "<session>:<window index>" for a window, else the session name.
func (s SessionView) Title() string {
	switch {
	case s.Label != "":
		return s.Label
	case s.Window != "":
		return s.Session + ":" + strconv.Itoa(s.Index)
	}
	return s.Session
}

// HasRenderedView reports whether the entry has a rendered view: a connected
// v2 host reporting Claude Code in its pane.
func (s SessionView) HasRenderedView() bool { return s.Online && s.Rendered && s.Claude }

// CanPaste reports whether a message can be sent into the entry's active pane
// from the Terminal view (PasteActivePane): the host is connected and
// -interactive, and either pastes into any pane or — an older muster-mirror —
// reports Claude Code in this one, which its Claude-only paste reaches.
func (s SessionView) CanPaste() bool {
	if !s.Online || !s.Interactive {
		return false
	}
	return s.PanePaste || (s.Rendered && s.Claude)
}

// shells are the commands Kind calls a shell.
var shells = []string{"ash", "bash", "csh", "dash", "fish", "ksh", "nu", "pwsh", "sh", "tcsh", "xonsh", "zsh"}

// The kinds Kind names itself. Anything else it returns is the command.
const (
	KindClaudeCode = "Claude Code"
	KindOpenCode   = "opencode"
	KindShell      = "shell"
)

// Kind is what runs in the entry's pane, as a list names it: KindClaudeCode
// when the host resolved a Claude Code session there, KindOpenCode / KindShell
// from the pane's command, else the command itself; "" when the host said
// nothing (a per-session host, which reports no command).
func (s SessionView) Kind() string {
	switch {
	case s.Claude:
		return KindClaudeCode
	case s.Command == "opencode":
		return KindOpenCode
	case slices.Contains(shells, s.Command):
		return KindShell
	}
	return s.Command
}

// Entries is every entry of the host, in its own stable order: a per-window
// host's windows (session, then index), a per-session host's sessions (name).
func (h HostView) Entries() []SessionView {
	base := SessionView{Host: h.Host, Online: h.Online, Interactive: h.Interactive, Rendered: h.Rendered, PanePaste: h.PanePaste}
	if h.Windows != nil {
		out := make([]SessionView, 0, len(h.Windows))
		for _, w := range h.Windows {
			e := base
			e.Session, e.Window, e.Index, e.Active = w.Session, w.ID, w.Index, w.Active
			e.Label, e.Command, e.Activity, e.Claude = w.Label, w.Command, w.Activity, w.Claude
			out = append(out, e)
		}
		return out
	}
	out := make([]SessionView, 0, len(h.Sessions))
	for _, s := range h.Sessions {
		e := base
		e.Session, e.Active = s, true
		e.Label, e.Activity, e.Claude = h.Labels[s], h.Activity[s], slices.Contains(h.Claude, s)
		out = append(out, e)
	}
	return out
}

// Find is the host's entry for (session, window). window == "" is the
// session's ACTIVE window — what a URL from before windows were addressable
// names — and on a per-session host is the only address there is.
//
// 🔴 THE SAME RULE AS THE HUB'S OWN RESOLUTION (hostConn.target), over the same
// list: a page and the request it then makes cannot resolve one address to two
// windows.
func (h HostView) Find(session, window string) (SessionView, bool) {
	var first SessionView
	found := false
	for _, e := range h.Entries() {
		if e.Session != session {
			continue
		}
		switch {
		case window != "":
			if e.Window == window {
				return e, true
			}
		case e.Active:
			return e, true
		case !found:
			first, found = e, true
		}
	}
	return first, found
}

// CanPaste reports whether a message can be sent into session's active pane
// from the Terminal view. See SessionView.CanPaste.
func (v HostView) CanPaste(session string) bool {
	e, ok := v.Find(session, "")
	return ok && e.CanPaste()
}

// Ordered is every entry of hosts, as ONE list across hosts, MOST RECENTLY
// ACTIVE FIRST.
//
// The sort is STABLE over the order mirrored entries always had — hosts as
// given (Hub.Hosts sorts them by name), then each host's own order
// (HostView.Entries) — so entries active in the same ActivityBucket keep that
// order, and every entry whose host reported no activity (an older
// muster-mirror) comes after all that did, in that same order. With no
// activity anywhere the result is exactly the old list.
//
// 🔴 ONE RULE, ONE PLACE. The Agents view renders this list and the hub
// decides from this same list whether a host's update changed anything worth
// re-rendering (orderKeys); a second copy of the comparison would let the two
// disagree about what "the order changed" means.
func Ordered(hosts []HostView) []SessionView {
	var out []SessionView
	for _, h := range hosts {
		out = append(out, h.Entries()...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Activity > out[j].Activity })
	return out
}

// orderKeys is Ordered's sequence as comparable keys: which entry is where.
func orderKeys(hosts []HostView) []string {
	list := Ordered(hosts)
	keys := make([]string, len(list))
	for i, s := range list {
		keys[i] = s.Host + "\x00" + s.Session + "\x00" + s.Window
	}
	return keys
}
