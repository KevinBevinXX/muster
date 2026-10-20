package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE RENDERED VIEW OF A HERMES WINDOW (docs/design/hermes.md, Phase 2): the
// watch protocol a Claude Code view uses, fed from Hermes's state.db instead
// of a transcript file. Msg.Gen is the conversation's lineage root (a
// rotation does not reset the page); Msg.Offset is a messages.id, the tail's
// cursor. Read-only: it reads state.db and writes nothing anywhere.
//
// 🔴 A HERMES VIEW NEVER RESUMES. A reconnect always gets a Reset backfill:
// an in-place rewrite (transcript_revision) moves no id, so a cursor the page
// kept cannot prove the rows before it are unchanged.

// HermesSourcer is a backend that resolves Hermes windows' sessions (Tmux
// with a HermesSource).
type HermesSourcer interface {
	HermesSource() *HermesSource
}

// HermesSource implements HermesSourcer; nil when t resolves none.
func (t Tmux) HermesSource() *HermesSource { return t.Hermes }

// errNoHermesViewNow ends a view whose window no longer shows a Hermes
// conversation it can read: the CLI exited, or the terminal switched sessions
// (/resume, /branch), or its breadcrumb no longer resolves.
var errNoHermesViewNow = errors.New("this window no longer shows a Hermes conversation muster can read (the CLI exited, or the terminal switched sessions with /resume or /branch)")

// hermesTailRows bounds one tail read; a fuller page is read again at once.
const hermesTailRows = 256

// hermesWindow is the Hermes window m names, read NOW, with the session it
// shows; false when it is not one with a rendered view.
func (h *hostSide) hermesWindow(ctx context.Context, m mirror.Msg) (AgentPane, bool) {
	if m.Window == "" {
		return AgentPane{}, false
	}
	w, err := h.window(ctx, m.Session, m.Window)
	if err != nil || w.Claude != nil || w.Agent == nil || w.Agent.Runtime != mirror.RuntimeHermes || w.Agent.Gen == "" {
		return AgentPane{}, false
	}
	return *w.Agent, true
}

// hermesTail serves one watch of a Hermes window until ctx ends (nil) or the
// view cannot continue.
func (h *hostSide) hermesTail(ctx context.Context, src *HermesSource, m mirror.Msg) error {
	p, ok := h.hermesWindow(ctx, m)
	if !ok {
		return errNoHermesViewNow
	}
	st := src.State()
	var (
		root    = p.Gen
		busy    = p.Busy
		cursor  int64
		fp      hermeshost.Fingerprint
		lineage hermeshost.Lineage
	)
	send := func(msgs []mirror.Msg) error {
		for _, b := range msgs {
			b.Type, b.ID, b.Gen = mirror.TypeEvents, m.ID, root
			if h.c.cfg.NoPromptIndex {
				for i := range b.Events {
					b.Events[i].Line = 0
				}
			}
			if err := h.send(b); err != nil {
				return err
			}
		}
		return nil
	}
	start := func() error {
		page, err := st.Backfill(ctx, root, mirror.MaxBackfillEvents)
		if err != nil {
			return fmt.Errorf("read the Hermes conversation: %w", err)
		}
		cursor, fp, lineage = page.Cursor, page.Fingerprint, page.Lineage
		lines := renderRows(page.Rows)
		// The event bound is the Claude view's: the newest MaxBackfillEvents.
		lines = lastEvents(lines, mirror.MaxBackfillEvents)
		// 🔴 Redacted before it is framed (redact_stream.go).
		redactLines(lines)
		batches := mirror.BatchLines(lines, 0)
		if len(batches) == 0 {
			batches = []mirror.Msg{{}}
		}
		batches[0].Reset = true
		batches[len(batches)-1].Offset = cursor
		batches[len(batches)-1].Busy = busy
		if err := send(batches); err != nil {
			return err
		}
		if h.c.cfg.NoPromptIndex {
			return nil
		}
		refs := page.Prompts
		redactPrompts(refs)
		refs, more := mirror.BoundPrompts(refs)
		return send([]mirror.Msg{{Offset: cursor, Busy: busy, PromptIndex: true, Prompts: refs, PromptsMore: more}})
	}
	if err := start(); err != nil {
		return err
	}
	t := time.NewTicker(h.c.cfg.ViewPoll)
	defer t.Stop()
	for tick := 1; ; tick++ {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if tick%reresolveEvery == 0 {
			np, ok := h.hermesWindow(ctx, m)
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return errNoHermesViewNow
			}
			if np.Gen != root {
				// /new, or a restart of the CLI: another conversation.
				root, busy = np.Gen, np.Busy
				if err := start(); err != nil {
					return err
				}
				continue
			}
			if np.Busy != busy {
				busy = np.Busy
				if err := send([]mirror.Msg{{Offset: cursor, Busy: busy}}); err != nil {
					return err
				}
			}
		}
		var rows []hermeshost.Row
		var last hermeshost.Page
		reset := false
		for {
			page, err := st.Tail(ctx, root, cursor, hermesTailRows)
			if err != nil {
				return fmt.Errorf("read the Hermes conversation: %w", err)
			}
			if !hermeshost.SameLineage(page.Lineage, lineage) {
				reset = true // a rotation: start over on the new lineage
				break
			}
			rows = append(rows, page.Rows...)
			last = page
			if len(page.Rows) < hermesTailRows {
				break
			}
			cursor = page.Cursor
		}
		// 🔴 A fingerprint the appended rows do not explain is a rewind, a
		// compaction or an in-place rewrite: the rows already drawn are no
		// longer the conversation. Start over.
		if reset || last.Fingerprint != fp.After(rows) {
			if err := start(); err != nil {
				return err
			}
			continue
		}
		cursor, fp = last.Cursor, last.Fingerprint
		if len(rows) == 0 {
			continue
		}
		var drawn []hermeshost.Row
		for _, r := range rows {
			if r.Active {
				drawn = append(drawn, r)
			}
		}
		lines := renderRows(drawn)
		redactLines(lines)
		batches := mirror.BatchLines(lines, 0)
		if len(batches) == 0 {
			continue
		}
		batches[len(batches)-1].Offset = cursor
		for i := range batches {
			batches[i].Busy = busy
		}
		if err := send(batches); err != nil {
			return err
		}
	}
}

// renderRows is rows as cctranscript lines: one per row that draws anything,
// its End the row's id (the cursor a batch ending there resumes past).
func renderRows(rows []hermeshost.Row) []cctranscript.Line {
	var out []cctranscript.Line
	for _, r := range rows {
		if evs := hermeshost.Render(r); len(evs) > 0 {
			out = append(out, cctranscript.Line{Events: evs, End: r.ID})
		}
	}
	return out
}

// lastEvents is the newest lines holding at most n events in all (a line is
// kept whole or not at all).
func lastEvents(lines []cctranscript.Line, n int) []cctranscript.Line {
	total := 0
	for i := len(lines) - 1; i >= 0; i-- {
		total += len(lines[i].Events)
		if total > n {
			return lines[i+1:]
		}
	}
	return lines
}
