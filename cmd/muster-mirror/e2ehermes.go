//go:build e2emirror

package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// HERMES WINDOWS IN THE BROWSER SUITE (docs/design/hermes.md, Phase 2), for a
// per-window fake (MUSTER_MIRROR_E2E_WINDOWS_FILE) with
// MUSTER_MIRROR_E2E_HERMES_HOME set:
//
//   - the fake creates <home>/state.db from Hermes's schema at the pin
//     (hermestest.Create) and plays Hermes's GATEWAY: on every listing it
//     inserts the lines of <home>/rows.jsonl it has not inserted yet, each
//     {"session", "role", "content", "tool_calls", "tool_call_id",
//     "display_kind"} — a session row is made for a session the first time it
//     is named — so a spec "writes a turn" by appending a line;
//
//   - MUSTER_MIRROR_E2E_HERMES_WINDOWS_FILE adds Hermes windows to the
//     listing, one a line:
//
//     <session>|<window id>|<index>|<active 0/1>|<activity unix secs>|<hermes session id, or empty>|<status>
//
//     An empty session id is a window whose breadcrumb is stale: listed as
//     Hermes, with no rendered view. The fake plays the locator here; the
//     transcript is read by the REAL reader (mirrorhost's Hermes tail) from the
//     real state.db.
var e2eHermes struct {
	once    sync.Once
	src     *mirrorhost.HermesSource
	mu      sync.Mutex
	db      *sql.DB
	applied int
	known   map[string]bool
}

// hermesSourceE2E is the fake's HermesSource, nil without a Hermes home.
func hermesSourceE2E() *mirrorhost.HermesSource {
	home := os.Getenv("MUSTER_MIRROR_E2E_HERMES_HOME")
	if home == "" {
		return nil
	}
	e2eHermes.once.Do(func() {
		db, err := hermestest.Create(home)
		if err != nil {
			panic("muster-mirror (e2emirror): create the Hermes state.db: " + err.Error())
		}
		e2eHermes.db, e2eHermes.known = db, map[string]bool{}
		e2eHermes.src = mirrorhost.NewHermesSource(home, filepath.Join(home, "muster-status"))
	})
	return e2eHermes.src
}

// HermesSource implements mirrorhost.HermesSourcer.
func (w windowFake) HermesSource() *mirrorhost.HermesSource { return hermesSourceE2E() }

// syncHermesRows inserts rows.jsonl's new lines into state.db.
func syncHermesRows() {
	home := os.Getenv("MUSTER_MIRROR_E2E_HERMES_HOME")
	if hermesSourceE2E() == nil {
		return
	}
	e2eHermes.mu.Lock()
	defer e2eHermes.mu.Unlock()
	f, err := os.Open(filepath.Join(home, "rows.jsonl"))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		if n <= e2eHermes.applied {
			continue
		}
		var r struct {
			Session     string `json:"session"`
			Role        string `json:"role"`
			Content     string `json:"content"`
			ToolCalls   string `json:"tool_calls"`
			ToolCallID  string `json:"tool_call_id"`
			DisplayKind string `json:"display_kind"`
		}
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.Session == "" {
			e2eHermes.applied = n
			continue
		}
		now := float64(time.Now().UnixNano()) / 1e9
		if !e2eHermes.known[r.Session] {
			_, _ = e2eHermes.db.Exec(`INSERT OR IGNORE INTO sessions (id, source, started_at) VALUES (?, 'cli', ?)`, r.Session, now)
			e2eHermes.known[r.Session] = true
		}
		null := func(s string) any {
			if s == "" {
				return nil
			}
			return s
		}
		_, _ = e2eHermes.db.Exec(`INSERT INTO messages (session_id, role, content, tool_calls, tool_call_id, display_kind, timestamp)
VALUES (?, ?, ?, ?, ?, ?, ?)`, r.Session, r.Role, null(r.Content), null(r.ToolCalls), null(r.ToolCallID), null(r.DisplayKind), now)
		e2eHermes.applied = n
	}
}

// hermesWindowsE2E is MUSTER_MIRROR_E2E_HERMES_WINDOWS_FILE's windows.
func hermesWindowsE2E(context.Context) []mirrorhost.WindowInfo {
	file := os.Getenv("MUSTER_MIRROR_E2E_HERMES_WINDOWS_FILE")
	if file == "" {
		return nil
	}
	syncHermesRows()
	b, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var out []mirrorhost.WindowInfo
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) != 7 {
			continue
		}
		index, _ := strconv.Atoi(f[2])
		activity, _ := strconv.ParseInt(f[4], 10, 64)
		pane := windowFakePane(f[0], f[1])
		status := mirror.NormalizeStatus(f[6])
		var since int64
		if status != "" {
			since = 1700000000000
		}
		out = append(out, mirrorhost.WindowInfo{Session: f[0], ID: f[1], Index: index, Active: f[3] == "1", Command: "python3.14",
			Activity: activity, Pane: pane, PanePID: 1,
			Agent: &mirrorhost.AgentPane{Runtime: mirror.RuntimeHermes, Pane: pane, PID: 1, Gen: f[5], Status: status,
				StatusSince: since, Busy: status == mirror.StatusBusy, Source: mirrorhost.SourceBreadcrumb}})
	}
	return out
}

// e2eUnconfirmedSeen is the UNCONFIRMED-ONCE messages already answered
// "may have been submitted" (under e2eHermes.mu).
var e2eUnconfirmedSeen = map[string]bool{}

// SendHermes implements mirrorhost.HermesSender for the browser suite: the
// host's gated send with the fake playing Hermes. The REAL host side runs
// first (the -hermes-send switch, the window re-resolved through the real
// listing, the request's since); here the status is re-checked as the real
// send does — idle at exactly the server's since, else superseded — and a
// message that passes is written as the user row Hermes would write
// (rows.jsonl), which the real reader then shows. The screen, the Escapes
// and the database confirm are the live harness's (`make test-livehermes`).
func (w windowFake) SendHermes(ctx context.Context, req mirrorhost.HermesSendRequest) error {
	ap, err := req.Status(ctx)
	if err != nil || ap.Pane != req.Pane {
		return &mirrorhost.HermesSendError{Kind: mirrorhost.HermesSuperseded, Why: "the window is gone or no longer runs Hermes"}
	}
	if ap.Status != mirror.StatusIdle || ap.StatusSince != req.Since {
		return &mirrorhost.HermesSendError{Kind: mirrorhost.HermesSuperseded, Why: "Hermes's status changed after muster decided to send"}
	}
	if why := mirrorhost.HermesTextRefusal(req.Text); why != "" {
		return &mirrorhost.HermesSendError{Kind: mirrorhost.HermesNotSubmitted, Why: why}
	}
	// A message holding UNCONFIRMED-ONCE plays an Enter whose row never came,
	// the first time it is sent: "may have been submitted", nothing written.
	if strings.Contains(req.Text, "UNCONFIRMED-ONCE") {
		e2eHermes.mu.Lock()
		first := !e2eUnconfirmedSeen[req.Text]
		e2eUnconfirmedSeen[req.Text] = true
		e2eHermes.mu.Unlock()
		if first {
			return &mirrorhost.HermesSendError{Kind: mirrorhost.HermesUnconfirmed, Why: "Hermes recorded no new message within 8s of the Enter"}
		}
	}
	home := os.Getenv("MUSTER_MIRROR_E2E_HERMES_HOME")
	b, _ := json.Marshal(map[string]string{"session": ap.Gen, "role": "user", "content": strings.TrimSpace(req.Text)})
	f, err := os.OpenFile(filepath.Join(home, "rows.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return &mirrorhost.HermesSendError{Kind: mirrorhost.HermesNotSubmitted, Why: "the fake could not write the row"}
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
	return nil
}
