package mirror

import (
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
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
	// Activity is when the entry was last active (Unix seconds, floored to
	// ActivityBucket); 0 = the host did not say. For a Claude window it is its
	// last message when the host reported one (Window.CardActivity), else tmux's
	// window activity.
	Activity int64
	// Online, Interactive, Rendered, PanePaste, PaneScroll: the host's
	// (HostView). Claude:
	// the host reports Claude Code in this entry's pane.
	Online      bool
	Interactive bool
	Rendered    bool
	PanePaste   bool
	PaneScroll  bool
	Claude      bool
	// ClaudeSession is the Claude Code session id of the conversation in the
	// entry's pane (Window.ClaudeSession), "" for none — always "" for a
	// per-session host's entry. What its focus is keyed by.
	ClaudeSession string
	// Runtime is the window's non-Claude agent runtime (Window.Runtime:
	// RuntimeHermes, or ""), and AgentSession that runtime's session id
	// (Window.AgentSession), "" when the host names none — for Hermes, when
	// the window has no rendered view.
	Runtime      string
	AgentSession string
	// AgentNote is the host's line for the person about the window
	// (Window.AgentNote): why it has no rendered view, or what its credential
	// needs. Shown on its card.
	AgentNote string
	// Qualifier is what the entry's Title needs beside it to be told apart from
	// every OTHER entry listed with it ("" = its Title is already unique). Set
	// by Ordered (qualify) and only there; a SessionView from HostView.Entries
	// or Find alone has none — Qualified gives one the list's.
	Qualifier string
	// Repo is the short name of the repository the entry works in
	// (HostView.Repos, repos.go), "" when its host did not say.
	Repo string
	// Work is what the entry's Claude Code session has running now
	// (HostView.Work, work.go); the zero value when its host did not say.
	Work WindowWork
	// Color is the entry's tmux identity colour, lowercase #rrggbb
	// (HostView.Colors, work.go), "" when its host did not say.
	Color string
}

// QualifierSep joins a Title and its Qualifier: "mango:1 · laptop".
const QualifierSep = " · "

// DisplayTitle is the entry's name as every surface shows it — the card, the
// rendered view's and the terminal's header and <title>: its Title, then its
// Qualifier when it has one.
func (s SessionView) DisplayTitle() string {
	if s.Qualifier == "" {
		return s.Title()
	}
	return s.Title() + QualifierSep + s.Qualifier
}

// Address is the entry's tmux address on its host, as an operator types it:
// "<session>:<window index>" for a window, the session name for a per-session
// host's entry. It is unique WITHIN one host at any moment — tmux allows one
// session per name and one window per index in a session — which is what
// makes it the tie-break for two entries of one host that share a Title.
func (s SessionView) Address() string {
	if s.Window != "" {
		return s.Session + ":" + strconv.Itoa(s.Index)
	}
	return s.Session
}

// qualify sets every entry's Qualifier over list — the WHOLE list, across
// hosts, as Ordered produces it.
//
// 🔴 ONE RULE, ONE PLACE. The Agents card and the pages it opens (their header
// and their <title>) all take an entry's name from here, so they cannot
// disagree about whether "mango:1" needs its host. It is decided over the list
// as it is NOW, so a host connecting with a clashing label — or the one it
// clashed with going away — changes the answer on the next render; the hub
// re-renders on exactly those changes (a connect, a drop, a list change).
//
// The rule, over entries whose Title is identical (exact bytes):
//   - a Title no other entry has: no qualifier — the name is unchanged;
//   - a Title shared ACROSS hosts: each entry gets its host ("mango:1 · laptop");
//   - a Title shared by two entries of ONE host (two sessions whose label is
//     the same, say): each of those gets its tmux Address too, after the host
//     when the Title is also shared across hosts ("Gold:1 · laptop · scratch3:1"),
//     alone when it is not ("Gold:1 · scratch3:1"). Address is unique per host,
//     so every listed entry ends up with a distinct DisplayTitle.
func qualify(list []SessionView) {
	type hostTitle struct{ host, title string }
	byTitle := map[string]int{}
	hostsOf := map[string]map[string]bool{}
	byHostTitle := map[hostTitle]int{}
	for _, s := range list {
		t := s.Title()
		byTitle[t]++
		if hostsOf[t] == nil {
			hostsOf[t] = map[string]bool{}
		}
		hostsOf[t][s.Host] = true
		byHostTitle[hostTitle{s.Host, t}]++
	}
	for i := range list {
		t := list[i].Title()
		list[i].Qualifier = ""
		if byTitle[t] < 2 {
			continue
		}
		var parts []string
		if len(hostsOf[t]) > 1 {
			parts = append(parts, list[i].Host)
		}
		if byHostTitle[hostTitle{list[i].Host, t}] > 1 {
			parts = append(parts, list[i].Address())
		}
		list[i].Qualifier = strings.Join(parts, QualifierSep)
	}
}

// Qualified is e with the Qualifier Ordered(hosts) gives the same entry (host,
// session, window): what a PAGE of one entry calls it, so the page and the
// card it was opened from say the same name. An entry hosts does not list
// keeps no qualifier.
func Qualified(hosts []HostView, e SessionView) SessionView {
	e.Qualifier = ""
	for _, s := range Ordered(hosts) {
		if s.Host == e.Host && s.Session == e.Session && s.Window == e.Window {
			e.Qualifier = s.Qualifier
			break
		}
	}
	return e
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
// v2 host reporting Claude Code in its pane — or another runtime with a view,
// with the session it shows (AgentView).
func (s SessionView) HasRenderedView() bool {
	return s.Online && s.Rendered && (s.Claude || s.AgentView())
}

// SessionKey is the key the entry's conversation is stored by (a focus):
// a Claude session id as itself, a Hermes one runtime-qualified
// ("hermes:<id>", Window.SessionKey's rule); "" when the entry names none.
func (s SessionView) SessionKey() string {
	switch {
	case s.Claude:
		return s.ClaudeSession
	case s.AgentView():
		r, _ := s.Spec()
		return r.AgentKey(s.AgentSession)
	}
	return ""
}

// Spec is the RuntimeSpec of the entry's non-Claude runtime (Runtime), false
// for none or a Claude Code entry.
func (s SessionView) Spec() (RuntimeSpec, bool) {
	if s.Claude {
		return RuntimeSpec{}, false
	}
	return wireRuntime(s.Runtime)
}

// AgentView reports whether the entry is a window of a non-Claude runtime
// whose host names its conversation, and so has a rendered view (Hermes's,
// RuntimeSpec.HasView).
func (s SessionView) AgentView() bool {
	r, ok := s.Spec()
	return ok && s.AgentSession != "" && r.HasView(s.AgentSession)
}

// HermesView reports whether the entry is a Hermes window whose host names
// its conversation (AgentView, for Hermes).
func (s SessionView) HermesView() bool { return s.Runtime == RuntimeHermes && s.AgentView() }

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

// CanScroll reports whether the Terminal view may scroll the entry's pane
// through its history (Hub.ScrollWindow): the host is connected, -interactive,
// and advertised copy-mode scrolling. An older muster-mirror cannot, and the
// page then draws no gesture at all.
func (s SessionView) CanScroll() bool { return s.Online && s.Interactive && s.PaneScroll }

// shells are the commands Kind calls a shell.
var shells = []string{"ash", "bash", "csh", "dash", "fish", "ksh", "nu", "pwsh", "sh", "tcsh", "xonsh", "zsh"}

// The kinds Kind names itself. Anything else it returns is the command.
const (
	KindClaudeCode = "Claude Code"
	// KindHermes: the host reports a Hermes CLI in the pane
	// (Window.Runtime), whether or not it names the session.
	KindHermes   = "Hermes"
	KindOpenCode = "opencode"
	KindShell    = "shell"
)

// Kind is what runs in the entry's pane, as a list names it: KindClaudeCode
// when the host resolved a Claude Code session there, KindHermes when it
// reports a Hermes CLI (its command reads python3.14, S10), KindOpenCode / KindShell
// from the pane's command, else the command itself; "" when the host said
// nothing (a per-session host, which reports no command).
func (s SessionView) Kind() string {
	if s.Claude {
		return KindClaudeCode
	}
	if r, ok := s.Spec(); ok {
		return r.Kind
	}
	// A pane whose command is "opencode" is KindOpenCode by the fall-through
	// below: the label is the command.
	if slices.Contains(shells, s.Command) {
		return KindShell
	}
	return s.Command
}

// ActiveAt is the time the entry's card prints as "active … ago": its
// Activity (already floored to ActivityBucket by NormalizeActivity /
// NormalizeWindows), or zero for unknown so the card prints no "active" at all
// rather than "active 56y ago".
//
// 🔴 ONE RULE, ONE PLACE: the Agents view's card takes its time from here, and
// the hub decides from here (listed) whether an update moved a card's time.
func (s SessionView) ActiveAt() time.Time {
	if s.Activity <= 0 {
		return time.Time{}
	}
	return time.Unix(s.Activity, 0)
}

// Entries is every entry of the host, in its own stable order: a per-window
// host's windows (session, then index), a per-session host's sessions (name).
func (h HostView) Entries() []SessionView {
	base := SessionView{Host: h.Host, Online: h.Online, Interactive: h.Interactive, Rendered: h.Rendered, PanePaste: h.PanePaste, PaneScroll: h.PaneScroll}
	if h.Windows != nil {
		out := make([]SessionView, 0, len(h.Windows))
		for _, w := range h.Windows {
			e := base
			e.Session, e.Window, e.Index, e.Active = w.Session, w.ID, w.Index, w.Active
			// CardActivity: a Claude window's last MESSAGE when its host says,
			// else tmux's window activity — the one number the list orders by,
			// prints and decides the Idle section by.
			e.Label, e.Command, e.Activity, e.Claude, e.ClaudeSession = w.Label, w.Command, w.CardActivity(), w.Claude, w.ClaudeSession
			e.Runtime, e.AgentSession, e.AgentNote = w.Runtime, w.AgentSession, w.AgentNote
			e.Repo = h.Repos[w.ID]
			e.Work, e.Color = h.Work[w.ID], h.Colors[w.ID]
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
// decides from this same list (listed) whether a host's update changed what the
// view shows: the order, which it re-renders for at once, and each entry's
// ActiveAt, which it re-renders for throttled. A second copy of either
// comparison would let the two disagree about what "changed" means.
func Ordered(hosts []HostView) []SessionView {
	var out []SessionView
	for _, h := range hosts {
		out = append(out, h.Entries()...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Activity > out[j].Activity })
	qualify(out)
	return out
}

// listed is what Ordered shows, as comparables: keys is which entry is where,
// and activeAt[i] is the time keys[i]'s card prints (SessionView.ActiveAt, as
// Unix seconds; 0 = it prints none).
func listed(hosts []HostView) (keys []string, activeAt []int64) {
	list := Ordered(hosts)
	keys = make([]string, len(list))
	activeAt = make([]int64, len(list))
	for i, s := range list {
		keys[i] = s.Host + "\x00" + s.Session + "\x00" + s.Window
		if t := s.ActiveAt(); !t.IsZero() {
			activeAt[i] = t.Unix()
		}
	}
	return keys, activeAt
}
