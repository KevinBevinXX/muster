package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

// terminal is the input side: how a prompt reaches the interactive session.
// The production implementation is tmux (tmux.go); tests substitute a fake.
type terminal interface {
	// Paste delivers text into pane as ONE bracketed paste and submits it. It
	// fails with errPaneNotLive when the pane is gone, its process has exited, or
	// its id now names a pane on another tmux server — and ONLY BEFORE anything is
	// pasted: errPaneNotLive becomes a `503 not_ready`, which muster re-sends.
	// A failure after the paste is never errPaneNotLive (a failed Enter is
	// errPastedNotSubmitted, see afterPaste; a cancelled ctx is ctx.Err()).
	Paste(ctx context.Context, pane paneRef, text string) error
	// Enter presses Enter in pane once more (a submit the TUI may have missed),
	// with the same refusal as Paste.
	Enter(ctx context.Context, pane paneRef) error
	// Alive reports whether the session exists (zero pane) or, once there is a
	// pane, whether it still exists on the server it was created in.
	Alive(ctx context.Context, pane paneRef) error
}

// hookEvent is the subset of a Claude Code hook payload ccd reads. Field names are
// the CLI's (see testdata/hook_*.json, recorded from the pinned CLI).
type hookEvent struct {
	Event          string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	PromptID       string `json:"prompt_id"`
	Prompt         string `json:"prompt"`
	Source         string `json:"source"`
	// StopFailure only.
	Error                string `json:"error"`
	LastAssistantMessage string `json:"last_assistant_message"`
	// Notification only: which notification ("permission_prompt" is the one
	// read here).
	NotificationType string `json:"notification_type"`
}

type pendingTurn struct {
	// want is promptKey of the text ccd pasted: a UserPromptSubmit binds to this
	// turn only when its prompt has the same key (see onHook).
	want      string
	promptID  string
	submitted chan hookEvent // UserPromptSubmit
	done      chan hookEvent // Stop or StopFailure
}

type serverConfig struct {
	Bearer        string
	ConfigDir     string        // CLAUDE_CONFIG_DIR: transcripts must live under <it>/projects
	SubmitTimeout time.Duration // paste → UserPromptSubmit
	TurnTimeout   time.Duration // UserPromptSubmit → Stop/StopFailure
	// TranscriptGrace is how long to wait after Stop for the turn_duration record,
	// which the CLI writes only after its Stop hooks have run.
	TranscriptGrace time.Duration
	// HookLog, when set, is a file every raw hook payload is appended to — the
	// capture step for shapes not yet recorded (a rate-limited StopFailure).
	// ⚠ OFF BY DEFAULT, AND WHEN ON IT WRITES EVERY PROMPT'S TEXT TO THAT FILE (a
	// UserPromptSubmit payload carries the prompt) — on the volume when the path is
	// under it, as the live check's is. It is removed by the PR that closes the
	// OWED note in failure.go.
	HookLog string
	// HasCredential reports whether a credential the CLI would use is visible to
	// ccd; it only shapes the `session` field of /healthz (see sessionState).
	HasCredential func() bool
}

type server struct {
	cfg  serverConfig
	term terminal
	auth *authTracker

	slot chan struct{} // capacity 1: one ccd-driven turn at a time

	sup *supervisor // nil when ccd does not run the CLI itself
	// inputPane names the tmux pane prompts are pasted into, and whether there is
	// one to paste into now. Production wires the supervisor's (main.go); nil, or
	// a false answer, is not_ready — never a fallback to the session name, which
	// tmux resolves to whatever pane an attached operator last focused.
	inputPane func() (pane paneRef, ok bool)

	mu             sync.Mutex
	sessionStarted bool
	sessionEnded   bool // a SessionEnd arrived and no SessionStart since
	sessionID      string
	transcriptPath string // the session's transcript, as the last hook named it
	busy           bool   // a prompt was submitted (by anyone) and has not stopped
	// awaitingApproval: the TUI showed a permission prompt (Notification
	// permission_prompt) and no later hook says the session moved on. See
	// blocked.go.
	awaitingApproval bool
	// blocked reports awaitingApproval to muster; nil when the pod is not
	// offered muster's agent surface.
	blocked *blockedReporter
	pending *pendingTurn
}

func newServer(cfg serverConfig, term terminal, auth *authTracker) *server {
	if cfg.SubmitTimeout == 0 {
		cfg.SubmitTimeout = 20 * time.Second
	}
	if cfg.TurnTimeout == 0 {
		cfg.TurnTimeout = 30 * time.Minute
	}
	if cfg.TranscriptGrace == 0 {
		cfg.TranscriptGrace = 3 * time.Second
	}
	return &server{cfg: cfg, term: term, auth: auth, slot: make(chan struct{}, 1)}
}

// gatewayHandler is the NETWORK-facing mux. It deliberately does not serve /hook:
// a hook is a claim about what happened inside the session, and only a process in
// the pod may make it (see hookHandler).
func (s *server) gatewayHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/responses", s.handleResponses)
	// `/` is the same check: muster's default agent health path is "/"
	// (agentspec.DefaultGatewayHealthPath), which its renderer uses for the
	// startup and liveness probes. A per-kind health path for this image is muster
	// PR 2's concern; until then both answer identically.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /{$}", s.handleHealthz)
	return mux
}

// hookHandler is served ONLY on a loopback listener (see main.go). The hooks the
// CLI runs are `ccd hook <Event>` commands that POST their stdin here.
//
// ⚠ IT IS UNAUTHENTICATED, AND THAT IS A STATEMENT ABOUT WHO CAN REACH IT, NOT AN
// OVERSIGHT: anything that can connect to 127.0.0.1 in the pod can already type
// into the tmux session, which is strictly more than a forged Stop can do.
func (s *server) hookHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hook/{event}", s.handleHook)
	return mux
}

func (s *server) handleHook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	var ev hookEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "payload is not JSON", http.StatusBadRequest)
		return
	}
	if ev.Event == "" {
		ev.Event = r.PathValue("event")
	}
	if s.cfg.HookLog != "" {
		if f, err := os.OpenFile(s.cfg.HookLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			_, _ = f.Write(append(bytes.TrimSpace(body), '\n'))
			f.Close()
		}
	}
	s.onHook(ev)
	w.WriteHeader(http.StatusNoContent)
}

// onHook updates session state from one hook. It never blocks: a hook command that
// waited on ccd would stall the TUI that ran it.
func (s *server) onHook(ev hookEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.trackApprovalLocked(ev)
	switch strings.ToLower(ev.Event) {
	case "sessionstart":
		// SessionStart only fires once the CLI is past its first-run screens AND the
		// workspace trust dialog (hooks do not run in an untrusted workspace), so it
		// is the readiness signal that does not depend on reading the screen.
		s.sessionStarted, s.sessionEnded, s.sessionID, s.busy = true, false, ev.SessionID, false
		s.transcriptPath = ev.TranscriptPath
	case "sessionend":
		// The session is ending (an operator's /exit, after which the supervisor
		// restarts the CLI). Until the next SessionStart there is no prompt to
		// paste into.
		s.sessionStarted, s.sessionEnded, s.busy = false, true, false
	case "userpromptsubmit":
		s.busy = true
		if ev.TranscriptPath != "" {
			s.transcriptPath = ev.TranscriptPath
		}
		// Only the submit of the text ccd pasted is ccd's turn. Another one
		// arriving while a turn is pending (an operator submitting in the
		// attached terminal) is left unbound, so its reply is never returned as
		// this caller's.
		if p := s.pending; p != nil && p.promptID == "" && promptKey(ev.Prompt) == p.want {
			p.promptID = ev.PromptID
			select {
			case p.submitted <- ev:
			default:
			}
		}
	case "stop", "stopfailure":
		s.busy = false
		if p := s.pending; p != nil && p.promptID != "" && (ev.PromptID == "" || ev.PromptID == p.promptID) {
			select {
			case p.done <- ev:
			default:
			}
		}
		if strings.EqualFold(ev.Event, "stopfailure") && ev.Error != "" {
			s.auth.observeTurn(classifyAPIError(apiError{Code: ev.Error, Message: ev.LastAssistantMessage}))
		}
	}
}

// --- /v1/responses ---------------------------------------------------------

// The request is the OpenAI-Responses shape muster's client sends
// (internal/agents.responsesRequest). `model`, `instructions` and `tools` are
// accepted and IGNORED: the session's model comes from the CLI's own settings,
// its instructions from CLAUDE.md, and Claude Code cannot take function tools —
// a toolless answer is what muster's chat and kickoff paths ask for anyway.
type responsesRequest struct {
	Model  string          `json:"model"`
	Input  json.RawMessage `json:"input"`
	Stream *bool           `json:"stream"`
}

type inputItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// promptFrom extracts the LAST user message's text from `input`, which may be a
// bare string or an array of items whose content is a string or an array of
// {type: input_text|text, text} parts.
func promptFrom(raw json.RawMessage) (string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var items []inputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return "", errors.New("`input` is neither a string nor an array of items")
	}
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it.Role != "user" || (it.Type != "" && it.Type != "message") {
			continue
		}
		if json.Unmarshal(it.Content, &s) == nil {
			return s, nil
		}
		var parts []contentBlock
		if json.Unmarshal(it.Content, &parts) != nil {
			return "", errors.New("user message content is neither a string nor an array of parts")
		}
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "input_text" || p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String(), nil
	}
	return "", errors.New("`input` carries no user message")
}

// sanitizePrompt folds line endings and refuses control characters; handleResponses
// runs it BEFORE neutralizeInputMode, so the `!` check sees the text that will
// actually be pasted.
//
// 🔴 A CONTROL CHARACTER CAN END THE PASTE, AND WHAT FOLLOWS IT IS TYPED. tmux
// wraps the buffer in ESC[200~ … ESC[201~, and tmux 3.3a (the image's) and 3.4
// pass an ESC inside it through raw (measured; 3.7c sends it as the text "^["),
// so a prompt "\x1b[201~!touch X" closes the paste at its first byte and the `!`
// lands as TYPED input at an empty prompt — shell mode, past the neutraliser,
// which only ever sees a first byte of ESC (measured on the pinned CLI in the
// image: the file was created and no UserPromptSubmit fired). So every C0 control except LF and
// TAB is refused, and so are DEL and the C1 controls U+0080–U+009F (U+009B is a
// one-character CSI to a terminal that honours C1) — a typed 400 invalid_input,
// never a silent strip, so the caller learns the text was not sent.
//
// CR IS FOLDED, NOT REFUSED: CRLF and then any lone CR become LF. tmux turns every
// LF in a paste into CR anyway, so the pane receives the same bytes either way;
// folding makes the text ccd records (pendingTurn.want) the text the CLI reports
// (measured: a pasted "cr\rinside" came back in UserPromptSubmit as "cr\ninside"),
// and stops a CRLF arriving as TWO newlines.
func sanitizePrompt(prompt string) (string, error) {
	prompt = strings.ReplaceAll(prompt, "\r\n", "\n")
	prompt = strings.ReplaceAll(prompt, "\r", "\n")
	for i, r := range prompt {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return "", fmt.Errorf("the prompt contains control character U+%04X at byte %d; only "+
				"newline and tab are accepted (a control character could end the bracketed paste early)", r, i)
		}
	}
	return prompt, nil
}

// promptKey is how a UserPromptSubmit's `prompt` is matched to the text ccd
// pasted: equal after deleting every Unicode whitespace character from both.
//
// Exact equality was measured to be wrong for the pinned CLI, whose reported
// prompt differs from the pasted text in whitespace alone: a TAB comes back as
// four spaces; a trailing run of whitespace holding a space, tab or NBSP is
// trimmed whole ("endsp \n" → "endsp", "endnl\n  " → "endnl") while a run of
// newlines alone survived ("trail\nline2\n\n" intact); a lone CR comes back as
// LF. Leading spaces, inner blank lines and a 4194-byte multi-line paste came back
// verbatim. Deleting whitespace absorbs all of that and still separates any two
// prompts that differ in a non-whitespace character — which is the case this
// match exists for: an operator's own prompt is never bound to ccd's turn.
func promptKey(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// neutralizeInputMode keeps a leading `!` from becoming a shell command.
//
// 🔴 A PROMPT WHOSE FIRST BYTE IS `!` RUNS IN BASH INSIDE THE POD — EVEN AS A
// BRACKETED PASTE. Measured on the pinned CLI: a pasted "! pasted bang first" ran
// `pasted bang first` in bash (the TUI's shell mode, which no permission prompt
// and no approval hook sees) and fired no UserPromptSubmit. One leading space
// disarms it (measured: " ! …" reached the model as text, the space kept verbatim
// in the transcript).
//
// ONLY THE FIRST BYTE IS CHECKED, AND THAT IS MEASURED TO BE ENOUGH on the pinned
// CLI (2.1.296, in the image, tmux 3.3a, pasted exactly as ccd pastes, one fresh
// container per shape, 2026-10-10): a `!touch <marker>` behind each of "\n",
// "\t", " ", "\n\n", " \n", U+00A0 and U+3000 created no marker and fired
// UserPromptSubmit — a model turn, 0 of 7 — while the bare `!touch <marker>`
// positive control created its marker and fired no UserPromptSubmit, 2 of 2 (first
// and last). A CR never reaches the paste (sanitizePrompt folds it to LF). A
// different CLI version can decide differently: re-measure on a bump.
//
// 🔴 IT IS ONE OF TWO GUARDS, AND NEITHER IS ENOUGH ALONE. This one covers the
// paste's own first byte; it cannot see a `!` that a control sequence smuggles
// OUT of the paste ("\x1b[201~!…" starts with ESC) — sanitizePrompt refuses those.
// And sanitizePrompt alone would let a plain "!ls" through. The space is the only
// byte ccd ADDS to a prompt; the only bytes it CHANGES are CRs (sanitizePrompt).
//
// `/` IS DELIBERATELY LET THROUGH (operator decision, 2026-10-10: the session is
// a trusted operator's, sandboxed in its own pod). Measured on the pinned CLI: a
// known command ("/compact") runs locally and fires NO UserPromptSubmit (see
// runTurn's local_command failure); a `/` text that is not a command ("/etc/hosts
// is broken") fires UserPromptSubmit and is an ordinary model turn. `#`, `&`, `@`,
// `>` and `?` were measured too and are plain text.
func neutralizeInputMode(prompt string) string {
	if strings.HasPrefix(prompt, "!") {
		return " " + prompt
	}
	return prompt
}

func (s *server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if !bearerOK(r, s.cfg.Bearer) {
		writeFailure(w, &failure{Status: http.StatusUnauthorized, Type: failUnauthorized,
			Message: "missing or wrong bearer"})
		return
	}
	var req responsesRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeFailure(w, &failure{Status: http.StatusBadRequest, Type: failBadRequest, Message: "body: " + err.Error()})
		return
	}
	prompt, err := promptFrom(req.Input)
	if err != nil {
		writeFailure(w, &failure{Status: http.StatusBadRequest, Type: failBadRequest, Message: err.Error()})
		return
	}
	if prompt, err = sanitizePrompt(prompt); err != nil {
		writeFailure(w, &failure{Status: http.StatusBadRequest, Type: failInvalidInput, Message: err.Error()})
		return
	}
	if strings.TrimSpace(prompt) == "" {
		writeFailure(w, &failure{Status: http.StatusBadRequest, Type: failBadRequest, Message: "empty prompt"})
		return
	}
	prompt = neutralizeInputMode(prompt)

	res, f := s.runTurn(r.Context(), prompt)
	if f != nil {
		log.Printf("ccd: turn failed: %s (code=%q upstream=%d)", f.Error(), f.Code, f.Upstream)
		writeFailure(w, f)
		return
	}
	stream := req.Stream == nil || *req.Stream
	writeResponse(w, res.promptID, res.Segments, stream)
}

type turnOutcome struct {
	turnResult
	promptID string
}

// runTurn drives one prompt through the session and returns its reply or a
// failure. It holds the single turn slot for its whole duration.
func (s *server) runTurn(ctx context.Context, prompt string) (turnOutcome, *failure) {
	select {
	case s.slot <- struct{}{}:
		defer func() { <-s.slot }()
	default:
		return turnOutcome{}, &failure{Status: http.StatusConflict, Type: failBusy,
			Message: "another turn is already being driven through this session"}
	}

	if s.inputPane == nil {
		return turnOutcome{}, paneNotReady("ccd does not supervise the CLI (CCD_SUPERVISE is not 1), so it " +
			"knows no pane id to paste into, and it never falls back to the session name")
	}
	pane, ok := s.inputPane()
	if !ok {
		return turnOutcome{}, paneNotReady("the supervisor reports no running CLI in its pane (starting, " +
			"restarting, crash_loop or terminal_lost — see /healthz)")
	}

	p := &pendingTurn{want: promptKey(prompt), submitted: make(chan hookEvent, 1), done: make(chan hookEvent, 1)}
	s.mu.Lock()
	switch {
	case !s.sessionStarted:
		s.mu.Unlock()
		return turnOutcome{}, &failure{Status: http.StatusServiceUnavailable, Type: failNotReady,
			Message: "the session has not started (no SessionStart hook yet): the TUI may be on a " +
				"first-run, trust or login screen, or restarting — attach to the pane to see it"}
	case s.busy:
		s.mu.Unlock()
		return turnOutcome{}, &failure{Status: http.StatusConflict, Type: failBusy,
			Message: "the session is mid-turn (a prompt was submitted in the terminal)"}
	}
	s.pending = p
	// Where this turn's records can start: the transcript's size BEFORE the paste.
	// A long-lived session's transcript grows without bound, and readTurn re-reads
	// it while waiting for turn_duration, so it reads from here rather than from 0.
	offsetPath, offset := s.transcriptPath, int64(0)
	s.mu.Unlock()
	if offsetPath != "" {
		if fi, err := os.Stat(offsetPath); err == nil {
			offset = fi.Size()
		}
	}
	defer func() {
		s.mu.Lock()
		s.pending = nil
		s.mu.Unlock()
	}()

	if err := s.term.Paste(ctx, pane, prompt); err != nil {
		if errors.Is(err, errPaneNotLive) {
			return turnOutcome{}, paneNotReady(err.Error())
		}
		return turnOutcome{}, &failure{Status: http.StatusBadGateway, Type: failTerminal, Message: err.Error()}
	}

	// Wait for the CLI to accept the prompt. One extra Enter half-way through
	// covers an Enter that landed while the TUI was still ingesting the paste; an
	// extra Enter on an empty input line is a no-op in the TUI.
	//
	// ⚠ NOT FOR A `/` PROMPT: a slash command may have opened a picker or menu
	// (/model, /login), where an extra Enter would SELECT whatever is highlighted.
	slash := strings.HasPrefix(prompt, "/")
	var submitted hookEvent
	submitTimer := time.NewTimer(s.cfg.SubmitTimeout)
	defer submitTimer.Stop()
	retry := time.NewTimer(s.cfg.SubmitTimeout / 2)
	defer retry.Stop()
	if slash {
		retry.Stop()
	}
waitSubmit:
	for {
		select {
		case submitted = <-p.submitted:
			break waitSubmit
		case <-retry.C:
			_ = s.term.Enter(ctx, pane)
		case <-submitTimer.C:
			if slash {
				return turnOutcome{}, &failure{Status: http.StatusBadGateway, Type: failLocalCommand,
					Message: fmt.Sprintf("no UserPromptSubmit within %s of pasting a `/` prompt: the CLI most "+
						"likely ran it as a local slash command, which has no model turn and so no reply — "+
						"attach to the pane to see its output", s.cfg.SubmitTimeout)}
			}
			return turnOutcome{}, &failure{Status: http.StatusGatewayTimeout, Type: failNotSubmitted,
				Message: fmt.Sprintf("no UserPromptSubmit matching the pasted prompt within %s of the paste: the "+
					"prompt is sitting unsent in the input box, a dialog has focus, or the CLI reported a "+
					"different prompt (an operator's own submit is never taken for this turn's) — attach to "+
					"the pane", s.cfg.SubmitTimeout)}
		case <-ctx.Done():
			return turnOutcome{}, &failure{Status: http.StatusGatewayTimeout, Type: failTurnTimeout,
				Message: "caller went away before the prompt was submitted"}
		}
	}

	var stop hookEvent
	turnTimer := time.NewTimer(s.cfg.TurnTimeout)
	defer turnTimer.Stop()
	select {
	case stop = <-p.done:
	case <-turnTimer.C:
		return turnOutcome{}, &failure{Status: http.StatusGatewayTimeout, Type: failTurnTimeout,
			Message: fmt.Sprintf("no Stop/StopFailure within %s; the turn is still running in the session", s.cfg.TurnTimeout)}
	case <-ctx.Done():
		// ⚠ The turn KEEPS RUNNING in the TUI: ccd does not interrupt work the
		// operator may be watching. The session stays busy until its Stop arrives.
		return turnOutcome{}, &failure{Status: http.StatusGatewayTimeout, Type: failTurnTimeout,
			Message: "caller went away mid-turn; the turn is still running in the session"}
	}

	path := stop.TranscriptPath
	if path == "" {
		path = submitted.TranscriptPath
	}
	from := int64(0)
	if path == offsetPath {
		from = offset
	}
	res, err := s.readTurn(path, submitted.PromptID, from)
	if err != nil {
		return turnOutcome{}, &failure{Status: http.StatusBadGateway, Type: failTranscript, Message: err.Error()}
	}
	out := turnOutcome{turnResult: res, promptID: submitted.PromptID}

	// Failure precedence: the transcript's own error record, then the hook's.
	// Either one makes the turn a failure EVEN IF some text was also produced
	// before it — a partial answer delivered as a success hides the failure.
	if res.Err != nil {
		f := classifyAPIError(*res.Err)
		if f.Code == "" && stop.Error != "" {
			f.Code = stop.Error
		}
		s.auth.observeTurn(f)
		return turnOutcome{}, f
	}
	if strings.EqualFold(stop.Event, "StopFailure") {
		f := classifyAPIError(apiError{Code: stop.Error, Message: stop.LastAssistantMessage})
		s.auth.observeTurn(f)
		return turnOutcome{}, f
	}
	if strings.TrimSpace(res.Text()) == "" {
		return turnOutcome{}, &failure{Status: http.StatusBadGateway, Type: failEmptyReply,
			Message: fmt.Sprintf("the turn ended (Stop) with no assistant text (%d tool calls, transcript "+
				"complete=%v)", res.ToolUses, res.Complete)}
	}
	s.auth.observeTurn(nil)
	return out, nil
}

// paneNotReady is the not_ready answer for a pane ccd cannot or will not paste into.
func paneNotReady(why string) *failure {
	return &failure{Status: http.StatusServiceUnavailable, Type: failNotReady, Message: why}
}

// readTurn reads the turn for promptID, re-reading briefly until the CLI has
// written the turn_duration record that follows its Stop hooks.
func (s *server) readTurn(path, promptID string, from int64) (turnResult, error) {
	if path == "" || promptID == "" {
		return turnResult{}, errors.New("the hooks carried no transcript path or prompt id")
	}
	if err := s.underProjects(path); err != nil {
		return turnResult{}, err
	}
	deadline := time.Now().Add(s.cfg.TranscriptGrace)
	for {
		res, err := readTurnFile(path, promptID, from)
		if err == nil && res.Found && res.Complete {
			return res, nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return res, err
			}
			if !res.Found {
				return res, fmt.Errorf("prompt %s is not in %s", promptID, path)
			}
			return res, nil // no turn_duration yet: the records before it are complete
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// readTurnFile extracts the turn from byte offset `from` onwards, falling back to
// the whole file when the prompt is not found after it (the file was replaced or
// truncated since the offset was taken). A `from` that lands mid-record only
// costs that one unparseable line, which extractTurn skips.
func readTurnFile(path, promptID string, from int64) (turnResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return turnResult{}, err
	}
	defer f.Close()
	if from > 0 {
		if _, err := f.Seek(from, io.SeekStart); err == nil {
			if res, err := extractTurn(f, promptID); err != nil || res.Found {
				return res, err
			}
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return turnResult{}, err
		}
	}
	return extractTurn(f, promptID)
}

// underProjects refuses a transcript path outside <CLAUDE_CONFIG_DIR>/projects.
func (s *server) underProjects(path string) error {
	root := filepath.Join(s.cfg.ConfigDir, "projects")
	rel, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return fmt.Errorf("transcript path %q is not under %s", path, root)
	}
	return nil
}

// --- /healthz ---------------------------------------------------------------

// Session states reported on /healthz (never gating).
const (
	sessionStartedState = "started"       // SessionStart seen: the TUI is at its prompt
	sessionEndedState   = "ended"         // SessionEnd seen: the CLI is exiting or restarting
	sessionWaitLogin    = "waiting_login" // no SessionStart, and no credential visible (see below)
	sessionNotStarted   = "not_started"   // no SessionStart yet (first-run/trust screens, or still booting)
)

type health struct {
	OK       bool   `json:"ok"`
	Terminal string `json:"terminal"`
	// Supervisor is the supervised CLI's state; absent when ccd does not run it.
	Supervisor *cliState `json:"supervisor,omitempty"`
	Session    string    `json:"session"`
	Busy       bool      `json:"busy"`
	authSnapshot
}

// handleHealthz serves /healthz and / (identically). It answers 200 when ccd is
// up and the tmux session is alive, and non-200 only when one of these is not:
//   - tmux is not answering for the session (`tmux has-session` fails) or, once
//     the supervisor has started the CLI, the CLI's pane no longer exists on the
//     tmux server it was created in (tmuxTerminal.Alive);
//   - the supervised CLI is in crash_loop or terminal_lost (supervise.go) — so
//     the liveness probe restarts the pod.
//
// 🔴 SESSION AND AUTH ARE REPORTED, NEVER GATING. muster's renderer points the
// pod's startup and liveness probes at this path, so anything that fails it gets
// the pod restarted:
//   - waiting for SessionStart would kill a pod sitting at the /login screen
//     before an operator could attach and log in;
//   - a rate-limited account is healthy and resting, and an auth-failed one needs
//     a new token, not a restart — either would restart-loop a pod that cannot
//     fix itself by restarting.
//
// `auth` comes only from real turns (authTracker.observeTurn); ccd spends no
// request of its own to learn it.
func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	h := health{Terminal: "ok", authSnapshot: s.auth.snapshot()}
	var ref paneRef
	if s.sup != nil {
		st := s.sup.state()
		h.Supervisor = &st
		ref = s.sup.ref()
	}
	if err := s.term.Alive(ctx, ref); err != nil {
		h.Terminal = err.Error()
	}
	s.mu.Lock()
	started, ended := s.sessionStarted, s.sessionEnded
	h.Busy = s.busy
	s.mu.Unlock()
	h.Session = s.sessionState(started, ended)
	h.OK = h.Terminal == "ok" && (h.Supervisor == nil || (h.Supervisor.CLI != cliCrashLoop && h.Supervisor.CLI != cliLost))
	w.Header().Set("Content-Type", "application/json")
	if !h.OK {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(h)
}

// sessionState names where the TUI is. ⚠ `waiting_login` IS AN INFERENCE: before
// SessionStart ccd cannot see which screen the TUI is on (it never reads the
// screen), so it says `waiting_login` when it also cannot see any credential the
// CLI would use — the case in which the pinned CLI stops at its login-method
// screen (measured: with no token it never fires SessionStart).
func (s *server) sessionState(started, ended bool) string {
	switch {
	case started:
		return sessionStartedState
	case ended:
		return sessionEndedState
	case s.cfg.HasCredential != nil && !s.cfg.HasCredential():
		return sessionWaitLogin
	}
	return sessionNotStarted
}

// credentialVisible is the production HasCredential: CLAUDE_CODE_OAUTH_TOKEN in
// the environment ccd (and so the tmux server it starts) runs with, or the file
// an interactive /login writes, <CLAUDE_CONFIG_DIR>/.credentials.json. The file
// name is the one the pinned CLI binary carries; a /login writing it was not
// observed (that needs a real account).
func credentialVisible(configDir string) func() bool {
	return func() bool {
		if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
			return true
		}
		_, err := os.Stat(filepath.Join(configDir, ".credentials.json"))
		return err == nil
	}
}
