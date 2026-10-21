package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/redact"
)

// WHAT A WAITING HERMES WINDOW WAITS ON, AND ANSWERING ITS APPROVAL (Hermes
// Phase 4, docs/design/hermes.md, "Questions and approvals"). The host half.
//
// The hook says a session waits, and on what (Believed.Kind): an approval, a
// clarify or a sudo prompt. That is all a WAITING window's Pending says,
// unless this host answers approvals (-interactive and -hermes-answer):
// then, for a window waiting on an APPROVAL, it attaches Hermes's gateway
// viewer (internal/hermeshost's Viewer) to that session — the only place an
// approval's command lives (S5: no database row records it) — and lists the
// approval's command, description and the answers muster may send. It
// detaches as soon as the window no longer waits on an approval: muster must
// not be the reason a gateway outlives its panes (it idles out without
// clients).
//
// 🔴 "ONCE" ONLY FOR A COMMAND SHOWN WHOLE. A person approves what the card
// shows. A command that does not fit the card as one line — several lines,
// over the cap, or with what looks like a secret redacted out of it — is
// offered "deny" only, and the card says to approve it in the terminal.
//
// 🔴 A sudo prompt is never answered from muster, and a clarify is answered
// in the terminal (the viewer's answer to a clarify reaches only a viewer
// attached before Hermes asked it — measured, Phase 4 — which attaching on
// "waiting" never is).

// HermesViewer is one attached gateway viewer (hermeshost.Viewer), as the
// host uses it.
type HermesViewer interface {
	Respond(ctx context.Context, p hermeshost.ApprovalPrompt, choice string) error
	Events() <-chan hermeshost.ViewerEvent
	Done() <-chan struct{}
	Close()
}

// HermesViewerAttacher is a backend that can attach a gateway viewer to a
// Hermes session: Tmux through its HermesSource's home; the browser suite's
// fake its own.
type HermesViewerAttacher interface {
	AttachHermesViewer(ctx context.Context, session string) (HermesViewer, []hermeshost.ApprovalPrompt, error)
}

// AttachHermesViewer implements HermesViewerAttacher: the gateway of t's
// Hermes home. An error when t resolves no Hermes.
func (t Tmux) AttachHermesViewer(ctx context.Context, session string) (HermesViewer, []hermeshost.ApprovalPrompt, error) {
	if t.Hermes == nil {
		return nil, nil, errors.New("muster-mirror: this host resolves no Hermes home")
	}
	v, ps, err := t.Hermes.Home.Attach(ctx, session)
	if err != nil {
		return nil, nil, err
	}
	return v, ps, nil
}

// The words a waiting Hermes window's Pending says when there is no command
// to show (mirror.Pending.Summary).
const (
	HermesApprovalText     = "Hermes is waiting for an approval in the terminal"
	HermesSudoText         = "Hermes is asking for a sudo password in the terminal"
	HermesClarifyText      = "Hermes is asking a question in the terminal"
	hermesApprovalToolName = "terminal"
	// hermesCommandPrefix starts an approval's Summary: the command it would
	// run ("terminal: rm -r build").
	hermesCommandPrefix = "terminal: "
	// HermesNotWholeText is an approval's Detail when muster offers deny only
	// (approvable).
	HermesUnreachableText = "muster cannot reach Hermes's gateway on this host: answer it in the terminal."
	HermesNotWholeText    = "muster offers only Deny here: this is not a plain terminal command it can show whole (a file write, a plugin rule, too long, several lines, masked, or not plain ASCII). Approve it in the terminal, or deny it here."
)

// hermesTerminalReadTimeout bounds one read of a session's pending terminal
// calls (a short state.db read).
const hermesTerminalReadTimeout = 2 * time.Second

// hermesAttachTimeout bounds one attach (identify, a ticket, a WebSocket, a
// resume); hermesRetry is how long a failed attach waits before the next.
const (
	hermesAttachTimeout = 20 * time.Second
	hermesRetry         = 30 * time.Second
)

// hermesAsks is one connection's gateway viewers, by Hermes session id.
type hermesAsks struct {
	ctx      context.Context
	attacher HermesViewerAttacher // nil: this host does not answer approvals
	nudge    func()               // asks for a fresh listing (non-blocking)
	// terminal reads the commands of the `terminal` calls pending in a
	// session (hermeshost.State.PendingTerminalCommands); nil reads none, and
	// then no approval is offered "once".
	terminal func(ctx context.Context, session string) ([]string, error)
	logf     func(string, ...any)
	now      func() time.Time

	// clarify reads a session's pending clarify questions from state.db
	// (hermeshost.State.PendingClarify); nil reads none. It needs no viewer
	// and no -hermes-answer: it is the read the rendered view already makes.
	clarify func(ctx context.Context, session string) ([]hermeshost.ClarifyQuestion, error)

	mu       sync.Mutex
	sessions map[string]*hermesAsk
	// clarified caches each session's pending clarify by the status time it
	// was read at: one database read per wait, not per listing.
	clarified map[string]clarifyRead
}

// clarifyRead is one session's pending clarify, as read at since.
type clarifyRead struct {
	since int64
	p     *mirror.Pending
	// retryAt: the read failed; the words alone are shown, and it is read
	// again no sooner than this (zero for a read that succeeded).
	retryAt time.Time
}

// hermesAsk is one session's viewer: attaching, attached (viewer set), or
// refused (why set, retried after retryAt).
type hermesAsk struct {
	cancel    context.CancelFunc
	attaching bool
	viewer    HermesViewer
	prompts   []hermeshost.ApprovalPrompt
	// running is the commands of the `terminal` calls pending in the
	// conversation when prompts last changed (approvable).
	running []string
	why     string
	retryAt time.Time
}

// newHermesAsks is the asks of one connection; attacher nil for a host that
// does not answer approvals.
func newHermesAsks(ctx context.Context, attacher HermesViewerAttacher, nudge func(), logf func(string, ...any)) *hermesAsks {
	return &hermesAsks{ctx: ctx, attacher: attacher, nudge: nudge, logf: logf, now: time.Now, sessions: map[string]*hermesAsk{},
		clarified: map[string]clarifyRead{}}
}

// hermesClarifyTimeout bounds one clarify read (a short state.db read);
// hermesClarifyBackoff is how long a failed read waits before the next.
const (
	hermesClarifyTimeout = 2 * time.Second
	hermesClarifyBackoff = 30 * time.Second
)

// clarifyPending is what a window waiting on a clarify in session shows: its
// first question, the first question's choices, and how many more there are
// — read from the database once per wait (since). Its answer is the
// terminal's (no PromptID, no Choices).
func (m *hermesAsks) clarifyPending(session string, since int64) *mirror.Pending {
	p := &mirror.Pending{Kind: mirror.PendingClarify, Summary: HermesClarifyText, Tool: "clarify"}
	if m.clarify == nil || session == "" {
		return p
	}
	m.mu.Lock()
	c, ok := m.clarified[session]
	now := m.now()
	m.mu.Unlock()
	if ok && c.since == since && (c.retryAt.IsZero() || now.Before(c.retryAt)) {
		return c.p
	}
	ctx, cancel := context.WithTimeout(m.ctx, hermesClarifyTimeout)
	qs, err := m.clarify(ctx, session)
	cancel()
	if err != nil {
		// Off the listing's hot path: a failing read is cached too, with a
		// backoff, never retried on every listing.
		m.mu.Lock()
		m.clarified[session] = clarifyRead{since: since, p: p, retryAt: now.Add(hermesClarifyBackoff)}
		m.mu.Unlock()
		m.logf("muster-mirror: read Hermes session %s's pending clarify: %v (retried in %s)", session, err, hermesClarifyBackoff)
		return p
	}
	if len(qs) > 0 {
		p.Summary = redact.Secrets(qs[0].Question)
		var d []string
		if len(qs[0].Choices) > 0 {
			d = append(d, redact.Secrets(strings.Join(qs[0].Choices, " · ")))
		}
		if n := len(qs) - 1; n == 1 {
			d = append(d, "and 1 more question")
		} else if n > 1 {
			d = append(d, fmt.Sprintf("and %d more questions", n))
		}
		d = append(d, "answer in the terminal")
		p.Detail = strings.Join(d, " — ")
	}
	m.mu.Lock()
	m.clarified[session] = clarifyRead{since: since, p: p}
	m.mu.Unlock()
	return p
}

// attachPending sets each WAITING Hermes window's Pending (a copy of its
// AgentPane) to what it waits on, and keeps a viewer attached to each session
// waiting on an approval — and to no other. It never blocks on a gateway:
// a viewer attaches in the background and nudges a fresh listing.
func (m *hermesAsks) attachPending(windows []WindowInfo) {
	want, clarifying := map[string]bool{}, map[string]bool{}
	for i := range windows {
		a := windows[i].Agent
		if a == nil || a.Runtime != mirror.RuntimeHermes || a.Status != mirror.StatusWaiting || a.Hermes == nil {
			continue
		}
		var p *mirror.Pending
		switch a.Hermes.Status.Kind {
		case mirror.PendingApproval:
			if m.attacher != nil && a.Gen != "" {
				want[a.Gen] = true
			}
			p = m.approvalPending(a.Gen)
		case mirror.PendingSudo:
			p = &mirror.Pending{Kind: mirror.PendingSudo, Summary: HermesSudoText, Tool: "sudo"}
		case mirror.PendingClarify:
			clarifying[a.Gen] = true
			p = m.clarifyPending(a.Gen, a.StatusSince)
		default:
			continue
		}
		cp := *a
		cp.Pending = p
		windows[i].Agent = &cp
	}
	m.reconcile(want, clarifying)
}

// approvalPending is what a window waiting on an approval in session shows:
// the newest pending approval its viewer holds, or — no viewer, none yet —
// the words alone.
func (m *hermesAsks) approvalPending(session string) *mirror.Pending {
	p := &mirror.Pending{Kind: mirror.PendingApproval, Summary: HermesApprovalText, Tool: hermesApprovalToolName}
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.sessions[session]
	if a == nil {
		return p
	}
	if a.viewer == nil || len(a.prompts) == 0 {
		if a.why != "" {
			// Why is in the host's log; the card names no path or uid.
			p.Detail = HermesUnreachableText
		}
		return p
	}
	ap := a.prompts[len(a.prompts)-1]
	shown, choices := approvable(ap, a.running)
	p.Summary, p.Detail, p.PromptID = shown, redact.Secrets(ap.Description), ap.PromptID
	p.Choices = strings.Join(choices, " ")
	if !slices.Contains(choices, "once") {
		p.Detail = HermesNotWholeText
	}
	return p
}

// approvable is THE ONE RULE for a Hermes approval muster shows (audit #212):
// the card's line ("terminal: …", one line, capped, what looks like a secret
// redacted) and the answers muster offers — the listing (approvalPending) and
// the answer check (answer) both call it, and nothing else decides.
//
// "deny" whenever the approval offers it. "once" ONLY for an approval that is
// provably a terminal command whose command IS what will run — an allowlist,
// never a denylist of shapes:
//
//   - its description is one the pinned TERMINAL gate emits
//     (hermeshost.TerminalDescriptions): a write guard, computer use or
//     execute_code — whose "command" is a label — never is (a plugin's
//     rule CAN be: its description is the plugin's own text — the next rule);
//   - its command is not shaped like any pinned non-terminal gate's label
//     (hermeshost.NonTerminalLabel): a plugin rule's description is the
//     plugin's own text and can equal a terminal one, and a `terminal` call
//     whose command IS that label can be pending beside it (audit #212
//     round 3);
//   - its command equals the command of a `terminal` call pending in the
//     conversation (running, read from state.db: the RAW command), so it is
//     the command that runs, unredacted, unlabelled;
//   - and the card shows it whole: printable ASCII only (no control,
//     zero-width, homoglyph, combining or right-to-left character — any of
//     which the redactor can drop or a browser reorder), one line within the
//     cap, nothing muster's redactor would hide, none of the gateway's masks
//     (hermeshost.GatewayMasked), and no field the card does not render
//     (Unshown: a file edit's diff).
func approvable(ap hermeshost.ApprovalPrompt, running []string) (shown string, choices []string) {
	cmd := ap.Command
	red := redact.Secrets(cmd)
	shown = mirror.OneLine(hermesCommandPrefix+red, mirror.MaxPendingSummary)
	once := printableASCII(cmd) && red == cmd && shown == hermesCommandPrefix+cmd && !hermeshost.GatewayMasked(cmd) &&
		!ap.Unshown && hermeshost.TerminalDescriptions[ap.Description] && !hermeshost.NonTerminalLabel(cmd) && slices.Contains(running, cmd)
	for _, c := range mirror.HermesApprovalChoices {
		if !ap.Offers(c) || (c == "once" && !once) {
			continue
		}
		choices = append(choices, c)
	}
	return shown, choices
}

// printableASCII reports whether s is printable ASCII and spaces only.
func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// reconcile attaches a viewer to every session in want that has none (and
// whose last refusal is old enough) and detaches every other.
func (m *hermesAsks) reconcile(want, clarifying map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.attacher == nil {
		for s := range m.clarified {
			if !clarifying[s] {
				delete(m.clarified, s)
			}
		}
		return
	}
	for s, a := range m.sessions {
		if !want[s] {
			a.cancel()
			delete(m.sessions, s)
		}
	}
	for s := range m.clarified {
		if !clarifying[s] {
			delete(m.clarified, s)
		}
	}
	now := m.now()
	for s := range want {
		a := m.sessions[s]
		if a != nil && (a.attaching || a.viewer != nil || now.Before(a.retryAt)) {
			continue
		}
		if a != nil {
			a.cancel()
		}
		ctx, cancel := context.WithCancel(m.ctx)
		a = &hermesAsk{cancel: cancel, attaching: true}
		m.sessions[s] = a
		go m.follow(ctx, s, a)
	}
}

// follow attaches a's viewer to session and keeps its approvals current
// until ctx ends (detached) or the gateway hangs up.
func (m *hermesAsks) follow(ctx context.Context, session string, a *hermesAsk) {
	actx, cancel := context.WithTimeout(ctx, hermesAttachTimeout)
	v, ps, err := m.attacher.AttachHermesViewer(actx, session)
	cancel()
	m.mu.Lock()
	if m.sessions[session] != a || ctx.Err() != nil {
		m.mu.Unlock()
		if v != nil {
			v.Close()
		}
		return
	}
	if err != nil {
		a.attaching = false
		a.why, a.retryAt = mirror.OneLine(err.Error(), 160), m.now().Add(hermesRetry)
		m.mu.Unlock()
		m.logf("muster-mirror: no Hermes gateway viewer for session %s: %v (its approvals are answered in the terminal)", session, err)
		m.nudge()
		return
	}
	m.mu.Unlock()
	// 🟡 Still attaching while the pending calls are read (unlocked, up to
	// hermesTerminalReadTimeout): a listing in that gap must not start a
	// second attach (audit #212 round 3). And a detach in that gap replaced
	// or dropped a, so a is re-checked before it is written.
	running := m.readRunning(ctx, session)
	m.mu.Lock()
	if m.sessions[session] != a || ctx.Err() != nil {
		m.mu.Unlock()
		v.Close()
		return
	}
	a.attaching = false
	a.viewer, a.prompts, a.why, a.running = v, ps, "", running
	m.mu.Unlock()
	m.logf("muster-mirror: attached Hermes's gateway viewer to session %s (%d pending approval(s))", session, len(ps))
	m.nudge()
	defer v.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-v.Events():
			m.mu.Lock()
			if !ok {
				// The gateway hung up: drop the viewer; the next listing that
				// still wants one attaches again.
				if m.sessions[session] == a {
					a.viewer, a.prompts, a.retryAt = nil, nil, m.now()
				}
				m.mu.Unlock()
				m.nudge()
				return
			}
			fresh := false
			switch e.Kind {
			case hermeshost.EventApprovalRequest:
				a.prompts = append(slices.DeleteFunc(a.prompts, func(p hermeshost.ApprovalPrompt) bool { return p.PromptID == e.PromptID }), e.Prompt)
				fresh = true
			case hermeshost.EventApprovalSettled:
				a.prompts = slices.DeleteFunc(a.prompts, func(p hermeshost.ApprovalPrompt) bool { return p.PromptID == e.PromptID })
			}
			m.mu.Unlock()
			if fresh {
				running := m.readRunning(ctx, session)
				m.mu.Lock()
				a.running = running
				m.mu.Unlock()
			}
			m.nudge()
		}
	}
}

// readRunning is session's pending terminal commands now; nil when they
// cannot be read (then nothing is offered "once").
func (m *hermesAsks) readRunning(ctx context.Context, session string) []string {
	if m.terminal == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, hermesTerminalReadTimeout)
	defer cancel()
	cmds, err := m.terminal(ctx, session)
	if err != nil {
		m.logf("muster-mirror: read Hermes session %s's pending terminal calls: %v (no approval offered once)", session, err)
		return nil
	}
	return cmds
}

// close detaches every viewer.
func (m *hermesAsks) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for s, a := range m.sessions {
		a.cancel()
		delete(m.sessions, s)
	}
}

// The outcomes of answer, for the reply's refusal kind.
var (
	errHermesGone        = errors.New("Hermes is no longer waiting on that approval")
	errHermesUnreachable = errors.New("muster-mirror has no gateway viewer for this Hermes session")
	errHermesUnknown     = errors.New("the answer was sent to Hermes's gateway, which did not answer")
)

// answer answers the approval promptID of session with choice through its
// viewer — only an approval the viewer holds NOW, with an answer muster
// offers for it (approvable).
func (m *hermesAsks) answer(ctx context.Context, session, promptID, choice string) error {
	m.mu.Lock()
	a := m.sessions[session]
	var v HermesViewer
	var ap hermeshost.ApprovalPrompt
	found := false
	if a != nil && a.viewer != nil {
		v = a.viewer
		for _, p := range a.prompts {
			if p.PromptID == promptID {
				ap, found = p, true
			}
		}
	}
	m.mu.Unlock()
	switch {
	case v == nil:
		return errHermesUnreachable
	case !found:
		return errHermesGone
	}
	// THE SAME RULE as the listing, against the conversation as it is now.
	running := m.readRunning(ctx, session)
	if _, choices := approvable(ap, running); !slices.Contains(choices, choice) {
		return fmt.Errorf("%w with %q: muster does not offer that answer for it", errHermesGone, choice)
	}
	err := v.Respond(ctx, ap, choice)
	switch {
	case err == nil:
		m.mu.Lock()
		if m.sessions[session] == a {
			a.prompts = slices.DeleteFunc(a.prompts, func(p hermeshost.ApprovalPrompt) bool { return p.PromptID == promptID })
		}
		m.mu.Unlock()
		m.nudge()
		return nil
	case errors.Is(err, hermeshost.ErrApprovalRefused):
		return fmt.Errorf("%w (%v)", errHermesGone, err)
	case errors.Is(err, hermeshost.ErrViewerNoAnswer):
		return fmt.Errorf("%w (%v)", errHermesUnknown, err)
	}
	return fmt.Errorf("%w (%v)", errHermesUnreachable, err)
}

// hermesAttacher is what this connection attaches gateway viewers with: nil
// unless it answers Hermes approvals — an -interactive v2 host started with
// -hermes-answer whose backend resolves Hermes sessions and can attach.
func (h *hostSide) hermesAttacher() HermesViewerAttacher {
	if h.version < 2 || !h.c.cfg.Interactive || !h.c.cfg.HermesAnswer {
		return nil
	}
	if _, ok := h.hermesSource(); !ok {
		return nil
	}
	a, ok := capabilityOf[HermesViewerAttacher](h.c.cfg.Backend)
	if !ok {
		return nil
	}
	return a
}

// hermesAnswer is what this host advertises as Msg.HermesAnswer.
func (h *hostSide) hermesAnswer() bool { return h.hermes != nil && h.hermes.attacher != nil }

// nudge asks the refresh goroutine for a listing now, without waiting: a
// viewer's news reaches muster at once rather than at the next Refresh.
func (h *hostSide) nudge() {
	select {
	case h.kick <- make(chan struct{}):
	default:
	}
}

// answerHermes answers a Hermes approval (m.Runtime "hermes",
// m.HermesApproval) through the session's gateway viewer, and answers
// answered.
//
// 🔴 REFUSED UNLESS THIS HOST OPTED IN (-interactive and -hermes-answer),
// whatever muster asks — muster refuses it too — and only for a listed
// window whose pane runs Hermes NOW, waiting on an approval: the window is
// re-resolved here, and the session answered is the one its pane runs. Never
// a key into the pane.
func (h *hostSide) answerHermes(m mirror.Msg) {
	reply := func(err error) {
		ans := mirror.Msg{Type: mirror.TypeAnswered, ID: m.ID}
		if err != nil {
			ans.Error = err.Error()
			switch {
			case errors.Is(err, errHermesGone):
				ans.Refusal = mirror.AskRefusalHermesGone
			case errors.Is(err, errHermesUnreachable):
				ans.Refusal = mirror.AskRefusalHermesUnreachable
			case errors.Is(err, errHermesUnknown):
				ans.Refusal = mirror.AskRefusalHermesUnknown
			case errors.Is(err, errCannotAnswer):
				ans.Refusal = mirror.AskRefusalUnsupported
			}
			h.c.log.Printf("muster-mirror: Hermes approval in %q%s not answered: %v", m.Session, windowSuffix(m.Window), err)
		} else {
			h.c.log.Printf("muster-mirror: answered a Hermes approval in %q%s through its gateway (%s)", m.Session, windowSuffix(m.Window), m.HermesApproval.Choice)
		}
		_ = h.send(ans)
	}
	if !h.hermesAnswer() {
		reply(fmt.Errorf("%w: this host does not answer Hermes approvals (muster-mirror needs -interactive and -hermes-answer)", errCannotAnswer))
		return
	}
	if err := h.allowed(m); err != nil {
		reply(err)
		return
	}
	a := m.HermesApproval
	if a == nil || !slices.Contains(mirror.HermesApprovalChoices, a.Choice) || !mirror.ValidPromptID(a.PromptID) {
		reply(fmt.Errorf("%w: not an approval answer muster sends", errHermesGone))
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, 35*time.Second)
	defer cancel()
	wi, err := h.window(ctx, m.Session, m.Window)
	if err != nil {
		reply(fmt.Errorf("%w: %v", errHermesGone, err))
		return
	}
	ag := wi.Agent
	if ag == nil || ag.Runtime != mirror.RuntimeHermes || ag.Gen == "" || ag.Status != mirror.StatusWaiting ||
		ag.Hermes == nil || ag.Hermes.Status.Kind != mirror.PendingApproval {
		reply(fmt.Errorf("%w: the window's pane is not Hermes waiting on an approval now", errHermesGone))
		return
	}
	reply(h.hermes.answer(ctx, ag.Gen, a.PromptID, a.Choice))
}
