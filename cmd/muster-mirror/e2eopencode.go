//go:build e2emirror

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// OPENCODE WINDOWS IN THE BROWSER SUITE (docs/design/opencode.md), for a
// per-window fake (MUSTER_MIRROR_E2E_WINDOWS_FILE) with
// MUSTER_MIRROR_E2E_OPENCODE_DIR set:
//
//   - the fake starts a FAKE opencode v2 server in this process
//     (opencodetest) and holds a token it issued; the host's REAL
//     OpenCodeSource, client, title match and tail read it over HTTP, exactly
//     as they read a real server;
//
//   - on every listing it applies the lines of <dir>/ops.jsonl it has not
//     applied yet, each one of
//     {"op": "session", "id", "title", "parent"?},
//     {"op": "message", "session", "message": {…a Session.Message.Info…}} (a
//     message with an id already put is rewritten in place),
//     {"op": "status", "session", "status": "busy"|"idle"},
//     {"op": "rotate"} (the server's password changes: the token is refused);
//
//   - MUSTER_MIRROR_E2E_OPENCODE_WINDOWS_FILE adds opencode windows, one a
//     line:
//
//     <session>|<window id>|<index>|<active 0/1>|<activity unix secs>|<pane title>
//
//     The fake plays tmux (the pane and its title); the session is matched
//     from the title by the real matcher, against the fake server.
var e2eOpenCode struct {
	once    sync.Once
	srv     *opencodetest.Server
	src     *mirrorhost.OpenCodeSource
	mu      sync.Mutex
	applied int
}

// openCodeSourceE2E is the fake's OpenCodeSource, nil without a directory.
func openCodeSourceE2E() *mirrorhost.OpenCodeSource {
	if os.Getenv("MUSTER_MIRROR_E2E_OPENCODE_DIR") == "" {
		return nil
	}
	e2eOpenCode.once.Do(func() {
		e2eOpenCode.srv = opencodetest.New("e2e-fixture-password")
		creds, err := opencodehost.EnvCredentials(e2eOpenCode.srv.URL, e2eOpenCode.srv.Issue())
		if err != nil {
			panic("muster-mirror (e2emirror): the fake opencode token: " + err.Error())
		}
		e2eOpenCode.src = mirrorhost.NewOpenCodeSource(context.Background(), creds, "", log.New(os.Stderr, "", log.LstdFlags))
	})
	return e2eOpenCode.src
}

// OpenCodeSource implements mirrorhost.OpenCodeSourcer.
func (w windowFake) OpenCodeSource() *mirrorhost.OpenCodeSource { return openCodeSourceE2E() }

// syncOpenCodeOps applies ops.jsonl's new lines to the fake server.
func syncOpenCodeOps() {
	if openCodeSourceE2E() == nil {
		return
	}
	e2eOpenCode.mu.Lock()
	defer e2eOpenCode.mu.Unlock()
	f, err := os.Open(filepath.Join(os.Getenv("MUSTER_MIRROR_E2E_OPENCODE_DIR"), "ops.jsonl"))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		if n <= e2eOpenCode.applied {
			continue
		}
		e2eOpenCode.applied = n
		var op struct {
			Op      string         `json:"op"`
			ID      string         `json:"id"`
			Title   string         `json:"title"`
			Parent  string         `json:"parent"`
			Session string         `json:"session"`
			Status  string         `json:"status"`
			Message map[string]any `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &op) != nil {
			continue
		}
		srv := e2eOpenCode.srv
		switch op.Op {
		case "session":
			srv.PutSession(op.ID, op.Title, op.Parent)
		case "message":
			srv.PutMessage(op.Session, op.Message)
		case "status":
			srv.SetStatus(op.Session, op.Status)
		case "rotate":
			srv.Rotate("e2e-rotated-fixture-password")
		}
	}
}

// openCodeWindowsE2E is MUSTER_MIRROR_E2E_OPENCODE_WINDOWS_FILE's windows.
func openCodeWindowsE2E() []mirrorhost.WindowInfo {
	file := os.Getenv("MUSTER_MIRROR_E2E_OPENCODE_WINDOWS_FILE")
	if file == "" {
		return nil
	}
	syncOpenCodeOps()
	b, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var out []mirrorhost.WindowInfo
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.SplitN(strings.TrimRight(line, "\r"), "|", 6)
		if len(f) != 6 {
			continue
		}
		index, _ := strconv.Atoi(f[2])
		activity, _ := strconv.ParseInt(f[4], 10, 64)
		pane := windowFakePane(f[0], f[1])
		ap := mirrorhost.ResolveOpenCodePane(openCodeSourceE2E(), pane, 1, f[5])
		out = append(out, mirrorhost.WindowInfo{Session: f[0], ID: f[1], Index: index, Active: f[3] == "1", Command: "opencode",
			Activity: activity, Pane: pane, PanePID: 1, Agent: &ap})
	}
	return out
}
