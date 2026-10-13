package mirrorhost

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// MOST RECENTLY ACTIVE FIRST. muster lists mirrored sessions by when each was
// last active (mirror.Msg.Activity, mirror.Ordered); this is where a host reads
// that time.

// ActivityReader is a Backend that can say when each session was last active.
// Tmux is one; a backend that is not sends no activity, and muster lists its
// sessions in name order as before.
type ActivityReader interface {
	// Activity is session name → the Unix time of its last activity.
	Activity(ctx context.Context) (map[string]int64, error)
}

// ActivityReaderOf is b's ActivityReader, looking through wrappers (see ClaudeOf).
func ActivityReaderOf(b Backend) (ActivityReader, bool) { return capabilityOf[ActivityReader](b) }

// Activity implements ActivityReader with one `tmux list-windows -a`: a
// session's time is the NEWEST of its own #{session_activity} and every one of
// its windows' #{window_activity}.
//
// 🔴 #{session_activity} ALONE IS NOT "LAST ACTIVE". Measured on tmux 3.7c (a
// private server, no client attached, six readings over 13 s): a pane printing
// output moved its WINDOW's #{window_activity} to the second it printed — at
// two different times, and for a background window that was not the session's
// current one — while #{session_activity} stayed at the session's creation
// time throughout; `send-keys` into a pane moved the window's time too, and
// again not the session's. session_activity follows an attached CLIENT's keys,
// so on its own it would rank a session an agent has been working in for an
// hour below one the operator glanced at. Both are read, because a client's
// key that draws nothing (switching windows) is activity too.
//
// ⚠ "Active" is therefore "its screen changed, or someone typed": a pane that
// redraws on a timer (a clock, `watch`) is always recently active.
func (t Tmux) Activity(ctx context.Context) (map[string]int64, error) {
	cmd := exec.CommandContext(ctx, t.bin(), append(t.server(), "list-windows", "-a", "-F",
		"#{window_activity}\t#{session_activity}\t#{session_name}")...)
	cmd.Env = tmuxEnv()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tmux list-windows: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	return parseActivity(out.String()), nil
}

// parseActivity reads Activity's output: per session, the newest time on any
// of its lines. The session name is the LAST field, so it is taken whole
// whatever it holds; a field that is not a positive decimal number counts as
// no time, and a line with no usable time is skipped.
func parseActivity(out string) map[string]int64 {
	m := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 3)
		if len(f) != 3 || f[2] == "" {
			continue
		}
		var newest int64
		for _, field := range f[:2] {
			if v, err := strconv.ParseInt(field, 10, 64); err == nil && v > newest {
				newest = v
			}
		}
		if newest > m[f[2]] {
			m[f[2]] = newest
		}
	}
	return m
}

// SessionActivity is what a host SENDS for sessions: b's activity through
// mirror.NormalizeActivity (listed sessions only, floored to
// mirror.ActivityBucket, nothing later than now). nil when the backend cannot
// tell or the read failed — the error is returned for the caller to log, and
// muster then keeps the name order.
func SessionActivity(ctx context.Context, b Backend, sessions []string, now time.Time) (map[string]int64, error) {
	ar, ok := ActivityReaderOf(b)
	if !ok || len(sessions) == 0 {
		return nil, nil
	}
	raw, err := ar.Activity(ctx)
	if err != nil {
		return nil, err
	}
	return mirror.NormalizeActivity(raw, sessions, now.Unix()), nil
}
