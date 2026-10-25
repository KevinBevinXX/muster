package mirrorhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost"
)

// THE RENDERED VIEW OF AN OPENCODE WINDOW (docs/design/opencode.md): the watch
// protocol a Claude Code view uses, fed from the opencode server's API
// (GET /api/session/{id}/message) instead of a transcript file, re-read when
// the event stream names the session. Msg.Gen is the session id; Msg.Offset
// counts the lines this stream sent. Read-only: GETs, nothing else.
//
// 🔴 AN OPENCODE VIEW NEVER RESUMES, and starts over whenever what it drew
// is no longer the conversation: an event it already sent changed (an
// assistant message is rewritten in place while it streams, but
// opencodehost.Render only emits what is final), a drawn message is gone
// (a revert), or a message appeared before the newest one drawn.

// OpenCodeSourcer is a backend that resolves opencode windows' sessions (Tmux
// with an OpenCodeSource).
type OpenCodeSourcer interface {
	OpenCodeSource() *OpenCodeSource
}

// OpenCodeSource implements OpenCodeSourcer; nil when t resolves none.
func (t Tmux) OpenCodeSource() *OpenCodeSource { return t.OpenCode }

// openCodeSource is the backend's OpenCodeSource, when it has one AND this
// connection speaks v2.
func (h *hostSide) openCodeSource() (*OpenCodeSource, bool) {
	if h.version < 2 {
		return nil, false
	}
	s, ok := capabilityOf[OpenCodeSourcer](h.c.cfg.Backend)
	if !ok || s.OpenCodeSource() == nil {
		return nil, false
	}
	return s.OpenCodeSource(), true
}

// errNoOpenCodeViewNow ends a view whose window no longer shows an opencode
// session muster can read.
var errNoOpenCodeViewNow = errors.New("this window no longer shows an opencode session muster can read (the TUI exited, its title no longer names exactly one session, or the opencode server cannot be read)")

// errOpenCodeRefused ends a view whose server refused the token.
var errOpenCodeRefused = errors.New(unauthorizedWhy)

// The tail's bounds: the backfill reads the newest openCodeBackfill messages,
// a poll the newest openCodeTailPage, and a view that has drawn more than
// openCodeMaxDrawn messages starts over (which bounds what it remembers).
const (
	openCodeBackfill = 200
	openCodeTailPage = 64
	openCodeMaxDrawn = 2000
	// openCodeRecheck: a poll re-reads the session every this many ticks even
	// when no event named it (an event the stream dropped).
	openCodeRecheck = 10
)

// openCodeWindow is the opencode window m names, read NOW, with the session
// it shows; false when it is not one with a rendered view.
func (h *hostSide) openCodeWindow(ctx context.Context, m mirror.Msg) (AgentPane, bool) {
	if m.Window == "" {
		return AgentPane{}, false
	}
	w, err := h.window(ctx, m.Session, m.Window)
	if err != nil || w.Claude != nil || w.Agent == nil || w.Agent.Runtime != mirror.RuntimeOpenCode || w.Agent.Gen == "" {
		return AgentPane{}, false
	}
	return *w.Agent, true
}

// openCodeGone is why the window m names has no view now: the host's reason
// for the person (AgentPane.Note) when it is still an opencode window, else
// errNoOpenCodeViewNow.
func (h *hostSide) openCodeGone(ctx context.Context, m mirror.Msg) error {
	if w, err := h.window(ctx, m.Session, m.Window); err == nil && w.Agent != nil && w.Agent.Runtime == mirror.RuntimeOpenCode && w.Agent.Note != "" {
		return errors.New(w.Agent.Note)
	}
	return errNoOpenCodeViewNow
}

// openCodeTokenNotice is the view's row for what the token needs.
func openCodeTokenNotice(health string) cctranscript.Event {
	return cctranscript.Event{Kind: cctranscript.KindNotice, Notice: cctranscript.NoticeOpenCode, Status: "failed",
		Text: cctranscript.Cap(health, cctranscript.MaxTarget*4), Output: health}
}

// openCodeMatchedNotice is the first line of every opencode view: how the
// session was found, which the person must be able to see.
func openCodeMatchedNotice(p AgentPane) cctranscript.Event {
	title := ""
	if p.OpenCode != nil {
		title = p.OpenCode.Match.Title
	}
	return cctranscript.Event{Kind: cctranscript.KindNotice, Notice: cctranscript.NoticeOpenCode,
		Text: cctranscript.Cap("matched by title: "+title, cctranscript.MaxTarget*4),
		Output: "muster found this session by the pane's title, so it shows it read-only. " +
			"Type into opencode in the terminal."}
}

// openCodeTail serves one watch of an opencode window until ctx ends (nil) or
// the view cannot continue.
func (h *hostSide) openCodeTail(ctx context.Context, src *OpenCodeSource, m mirror.Msg) error {
	p, ok := h.openCodeWindow(ctx, m)
	if !ok {
		return h.openCodeGone(ctx, m)
	}
	var (
		id     = p.Gen
		busy   = p.Busy
		offset int64
		// drawn is each drawn message's emitted events, key → fingerprint;
		// order the drawn messages, oldest first, and pos each one's place.
		drawn map[string]map[string]string
		order []string
		pos   map[string]int
		seen  uint64
		polls int
		// health is the token line the view shows now (src.tokenHealth).
		health string
	)
	send := h.viewSend(m, func() string { return id })
	read := func(limit int) ([]opencodehost.Message, error) {
		cl, err := src.client()
		if err != nil {
			return nil, err
		}
		msgs, err := cl.Messages(ctx, id, limit)
		if errors.Is(err, opencodehost.ErrUnauthorized) {
			return nil, errOpenCodeRefused
		}
		if err != nil {
			return nil, fmt.Errorf("read the opencode session: %w", err)
		}
		return msgs, nil
	}
	// draw records msg as drawn and returns its events.
	draw := func(msg opencodehost.Message, ks []opencodehost.Keyed) []cctranscript.Event {
		if _, ok := drawn[msg.ID]; !ok {
			drawn[msg.ID] = map[string]string{}
			pos[msg.ID] = len(order)
			order = append(order, msg.ID)
		}
		evs := make([]cctranscript.Event, 0, len(ks))
		for _, k := range ks {
			drawn[msg.ID][k.Key] = fingerprint(k.Event)
			evs = append(evs, k.Event)
		}
		return evs
	}
	line := func(evs []cctranscript.Event) cctranscript.Line {
		offset++
		return cctranscript.Line{Events: evs, End: offset}
	}
	start := func() error {
		seen = src.changed(id)
		msgs, err := read(openCodeBackfill)
		if err != nil {
			return err
		}
		drawn, order, pos, offset = map[string]map[string]string{}, nil, map[string]int{}, 0
		var lines []cctranscript.Line
		// What the token needs comes first (Credentials.Health): a failing
		// renewal must be seen where the person reads the session.
		if health = src.tokenHealth(); health != "" {
			lines = append(lines, line([]cctranscript.Event{openCodeTokenNotice(health)}))
		}
		lines = append(lines, line([]cctranscript.Event{openCodeMatchedNotice(p)}))
		var refs []cctranscript.PromptRef
		for _, msg := range msgs {
			ks := opencodehost.Render(msg)
			evs := draw(msg, ks)
			if len(evs) == 0 {
				continue
			}
			for _, e := range evs {
				if e.Kind == cctranscript.KindPrompt && e.Line > 0 {
					refs = append(refs, cctranscript.PromptRef{Line: e.Line, Text: firstLine(e.Text)})
				}
			}
			lines = append(lines, line(evs))
		}
		lines = lastEvents(lines, mirror.MaxBackfillEvents)
		// 🔴 Redacted before it is framed (redact_stream.go).
		redactLines(lines)
		batches := mirror.BatchLines(lines, 0)
		if len(batches) == 0 {
			batches = []mirror.Msg{{}}
		}
		batches[0].Reset = true
		batches[len(batches)-1].Offset = offset
		for i := range batches {
			batches[i].Busy = busy
		}
		if err := send(batches); err != nil {
			return err
		}
		if h.c.cfg.NoPromptIndex {
			return nil
		}
		redactPrompts(refs)
		refs, more := mirror.BoundPrompts(refs)
		return send([]mirror.Msg{{Offset: offset, Busy: busy, PromptIndex: true, Prompts: refs, PromptsMore: more}})
	}
	return h.runView(ctx, renderedView{
		start: start,
		reresolve: func(ctx context.Context) (bool, error) {
			np, ok := h.openCodeWindow(ctx, m)
			if !ok {
				return false, h.openCodeGone(ctx, m)
			}
			if np.Gen != id {
				// The pane's title names another session now.
				id, busy, p = np.Gen, np.Busy, np
				return true, start()
			}
			// Busy is the event stream's, as every poll reads it.
			st, _ := src.status(id)
			return false, sendBusy(&busy, st == mirror.StatusBusy, offset, send)
		},
		poll: func() error {
			polls++
			if src.tokenHealth() != health {
				return start() // the token's line appeared, changed or cleared
			}
			st, _ := src.status(id)
			if c := src.changed(id); c == seen && polls%openCodeRecheck != 0 {
				return sendBusy(&busy, st == mirror.StatusBusy, offset, send)
			}
			seen = src.changed(id)
			msgs, err := read(openCodeTailPage)
			if err != nil {
				return err
			}
			restart, lines := openCodeDiff(msgs, drawn, pos, len(order))
			if restart || len(order) > openCodeMaxDrawn {
				return start()
			}
			var out []cctranscript.Line
			for _, l := range lines {
				if evs := draw(l.msg, l.keys); len(evs) > 0 {
					out = append(out, line(evs))
				}
			}
			nb := st == mirror.StatusBusy
			if len(out) == 0 {
				return sendBusy(&busy, nb, offset, send)
			}
			busy = nb
			// 🔴 Redacted before it is framed (redact_stream.go).
			redactLines(out)
			batches := mirror.BatchLines(out, 0)
			batches[len(batches)-1].Offset = offset
			for i := range batches {
				batches[i].Busy = busy
			}
			return send(batches)
		},
	})
}

// openCodeNew is one message's events a poll adds.
type openCodeNew struct {
	msg  opencodehost.Message
	keys []opencodehost.Keyed
}

// openCodeDiff compares a page of the newest messages (oldest first) with
// what the view drew — drawn (message → key → fingerprint), each drawn
// message's place in pos, n drawn in all — and returns what to append, or
// restart when the page says the drawn view is no longer the conversation:
//
//   - an event already sent changed, or is no longer rendered;
//   - a drawn message inside the page's span is missing (a revert);
//   - a message the view has not drawn comes before one it has (inserted);
//   - the page shares no message with the view (more than a page arrived
//     since the last read);
//   - an older message gained an event other than a tool result (a tool's
//     result may arrive after later items; anything else would be drawn out of
//     order).
func openCodeDiff(page []opencodehost.Message, drawn map[string]map[string]string, pos map[string]int, n int) (restart bool, add []openCodeNew) {
	if len(page) == 0 {
		return n > 0, nil
	}
	first := -1
	inPage := map[string]bool{}
	sawNew := false
	for _, msg := range page {
		inPage[msg.ID] = true
		keys, known := drawn[msg.ID]
		ks := opencodehost.Render(msg)
		if !known {
			if n > 0 && first < 0 {
				return true, nil // the page starts before, or apart from, what was drawn
			}
			sawNew = true
			add = append(add, openCodeNew{msg, ks})
			continue
		}
		if sawNew {
			return true, nil // a drawn message after one not drawn: something was inserted
		}
		if first < 0 {
			first = pos[msg.ID]
		}
		present := map[string]bool{}
		var fresh []opencodehost.Keyed
		for _, k := range ks {
			present[k.Key] = true
			fp, sent := keys[k.Key]
			switch {
			case sent && fp != fingerprint(k.Event):
				return true, nil
			case sent:
			case pos[msg.ID] != n-1 && !strings.HasPrefix(k.Key, "r:"):
				return true, nil
			default:
				fresh = append(fresh, k)
			}
		}
		for k := range keys {
			if !present[k] {
				return true, nil
			}
		}
		if len(fresh) > 0 {
			add = append(add, openCodeNew{msg, fresh})
		}
	}
	if n > 0 && first < 0 {
		return true, nil
	}
	// Every drawn message from the page's first on must still be in it.
	for mid, at := range pos {
		if first >= 0 && at >= first && !inPage[mid] {
			return true, nil
		}
	}
	return false, add
}

// fingerprint is an event's identity for "did it change".
func fingerprint(e cctranscript.Event) string {
	b, _ := json.Marshal(e)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

// firstLine is s's first non-blank line, trimmed.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}
