package hermeshost

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"

	"github.com/ZacxDev/muster/internal/mirror"
)

// Breadcrumb is a terminal's breadcrumb as Hermes writes it
// (P:hermes_cli/terminal_breadcrumbs.py, write_breadcrumb): the session the
// CLI on that terminal runs, and when it said so. Measured (S3): written at
// the CLI's first gateway attach and on /new; unchanged across compression
// (it keeps the logical root); left STALE by /resume and /branch; never
// deleted when the CLI exits (pruned after 30 days).
type Breadcrumb struct {
	SessionID string
	// TS is when Hermes wrote it, Unix seconds.
	TS float64
}

// ErrNoBreadcrumb: the terminal has no breadcrumb (Hermes's
// session.terminal_continue is off, or the CLI has not attached yet).
var ErrNoBreadcrumb = errors.New("no breadcrumb for this terminal")

// ReadBreadcrumb is the breadcrumb Hermes wrote for the terminal tty
// (/dev/pts/N). The session id must be one the wire carries
// (mirror.ValidAgentSession) and ts a finite positive time; anything else is
// an error, never a guess.
func (h Home) ReadBreadcrumb(tty string) (Breadcrumb, error) {
	b, err := h.readBreadcrumb(tty)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Breadcrumb{}, ErrNoBreadcrumb
		}
		return Breadcrumb{}, err
	}
	var raw struct {
		SessionID any `json:"session_id"`
		TS        any `json:"ts"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(b), "\xef\xbb\xbf")), &raw); err != nil {
		// Never %w the decoder's error: it quotes the file's bytes.
		return Breadcrumb{}, errors.New("the breadcrumb is not JSON")
	}
	id, _ := raw.SessionID.(string)
	id = strings.TrimSpace(id)
	if !mirror.ValidAgentSession(id) {
		return Breadcrumb{}, errors.New("the breadcrumb names no valid session id")
	}
	ts, ok := raw.TS.(float64)
	if !ok || math.IsNaN(ts) || math.IsInf(ts, 0) || ts <= 0 {
		return Breadcrumb{}, errors.New("the breadcrumb has no valid time")
	}
	return Breadcrumb{SessionID: id, TS: ts}, nil
}
