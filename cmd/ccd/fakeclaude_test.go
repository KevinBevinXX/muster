package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// fakeClaude is a stand-in for the interactive CLI, run INSIDE a real tmux pane
// by the tmuxit suite (CCD_TEST_AS=claude). It imitates exactly the behaviour ccd
// depends on, as measured on the real CLI:
//
//   - it blocks on a first-run screen unless .claude.json has
//     hasCompletedOnboarding, and on a trust screen unless projects[<cwd>] has
//     hasTrustDialogAccepted — and fires NO hooks while blocked;
//   - it enables bracketed paste, treats CR inside a paste as a newline (tmux
//     turns LF into CR on paste) and CR outside one as submit;
//   - it runs the hook COMMANDS from $CLAUDE_CONFIG_DIR/settings.json with
//     payloads shaped like the recorded ones (SessionStart, UserPromptSubmit,
//     Stop/StopFailure), so the image's real `ccd hook` wiring is exercised;
//   - it writes transcript records shaped like the recorded ones, the prompt
//     verbatim as received;
//   - a prompt whose first byte is `!` runs LOCALLY in `sh -c` (shell mode) —
//     including a `!` that arrives TYPED after a control sequence ended the
//     paste early, as on the real CLI — and so does a
//     `/` prompt naming a known command (fakeLocalCommands) — no UserPromptSubmit,
//     no model turn — while a `/` text naming no command ("/etc/hosts is broken")
//     is an ordinary prompt; all as measured on the real CLI. `/exit` fires
//     SessionEnd and exits 0;
//   - `--continue` resumes the newest transcript for the cwd (SessionStart source
//     "resume", same session id) and, with none, prints "No conversation found to
//     continue" and exits 1 — the real CLI's measured behaviour;
//   - CCD_FAKE_CRASH=1 makes it exit 3 at once (the supervisor's crash-loop test).
//
// Its reply states the prompt's length and SHA-256, so a test can check the
// bytes that ARRIVED from both ends: the transcript and the reply.
//
// ⚠ WHAT IT DOES NOT IMITATE: the real CLI expands a TAB to four spaces; this
// fake keeps it. That is a property of the CLI, not of ccd's paste, so the suite
// sends no tabs and the PR records the real CLI's behaviour instead.
// fakeLocalCommands are the slash commands the fake runs locally.
var fakeLocalCommands = map[string]bool{"/compact": true, "/help": true, "/clear": true, "/model": true}

func fakeClaude() int {
	if os.Getenv("CCD_FAKE_CRASH") == "1" {
		fmt.Print("fake crash\r\n")
		return 3
	}
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	cwd, _ := os.Getwd()
	// The flags ccd passes: --continue (the primary, before lanes), --resume
	// <id> (the primary once recorded, a lane once its transcript exists),
	// --session-id <id> (a lane's first start) and --model <m> (a lane's).
	var resume bool
	var resumeID, sessionID, model string
	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--continue":
			resume = true
		case "--resume", "--session-id", "--model":
			if i+1 >= len(os.Args) {
				fmt.Printf("%s needs a value\r\n", os.Args[i])
				return 1
			}
			v := os.Args[i+1]
			switch os.Args[i] {
			case "--resume":
				resumeID = v
			case "--session-id":
				sessionID = v
			default:
				model = v
			}
			i++
		default:
			fmt.Printf("unknown argument %q\r\n", os.Args[i])
			return 1
		}
	}
	st := map[string]any{}
	if b, err := os.ReadFile(filepath.Join(cfg, ".claude.json")); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	// stty acts on ITS stdin, which must be the pane's tty (the default is /dev/null).
	stty := exec.Command("stty", "raw", "-echo")
	stty.Stdin = os.Stdin
	if err := stty.Run(); err != nil {
		appendFile(filepath.Join(cfg, "fake-hooks.log"), "stty raw failed: "+err.Error()+"\n")
	}
	if st["hasCompletedOnboarding"] != true {
		fmt.Print("Welcome. Choose the text style that looks best with your terminal\r\n")
		blockForever()
	}
	projects, _ := st["projects"].(map[string]any)
	proj, _ := projects[cwd].(map[string]any)
	if proj == nil || proj["hasTrustDialogAccepted"] != true {
		fmt.Print("Quick safety check: Is this a project you created or one you trust?\r\n")
		blockForever()
	}

	// The real CLI's project-directory encoding, written out here independently of
	// ccd's own copy (hasPriorSession) so a wrong copy cannot agree with itself.
	projDir := filepath.Join(cfg, "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(cwd, "-"))
	sid, source := randomID(), "startup"
	if sessionID != "" {
		sid = sessionID
	}
	if resumeID != "" {
		if _, err := os.Stat(filepath.Join(projDir, resumeID+".jsonl")); err != nil {
			fmt.Printf("No conversation found with session ID: %s\r\n", resumeID)
			return 1
		}
		sid, source = resumeID, "resume"
	}
	appendFile(filepath.Join(cfg, "fake-argv.log"), fmt.Sprintf("sid=%s source=%s model=%s lane=%s args=%q\n",
		sid, source, model, os.Getenv("CCD_LANE"), os.Args[1:]))
	if resume {
		newest, newestAt := "", time.Time{}
		entries, _ := os.ReadDir(projDir)
		for _, e := range entries {
			if fi, err := e.Info(); err == nil && strings.HasSuffix(e.Name(), ".jsonl") && fi.ModTime().After(newestAt) {
				newest, newestAt = strings.TrimSuffix(e.Name(), ".jsonl"), fi.ModTime()
			}
		}
		if newest == "" {
			fmt.Print("No conversation found to continue\r\n")
			return 1
		}
		sid, source = newest, "resume"
	}
	transcript := filepath.Join(projDir, sid+".jsonl")
	_ = os.MkdirAll(projDir, 0o700)
	hooks := loadHooks(filepath.Join(cfg, "settings.json"))
	base := map[string]any{"session_id": sid, "transcript_path": transcript, "cwd": cwd}

	fire := func(event string, extra map[string]any) {
		payload := map[string]any{"hook_event_name": event}
		for k, v := range base {
			payload[k] = v
		}
		for k, v := range extra {
			payload[k] = v
		}
		b, _ := json.Marshal(payload)
		for _, c := range hooks[event] {
			cmd := exec.Command("sh", "-c", c)
			cmd.Stdin = bytes.NewReader(b)
			out, err := cmd.CombinedOutput()
			if err != nil || len(out) > 0 {
				// Diagnostics for the suite: a hook that failed or printed.
				appendFile(filepath.Join(cfg, "fake-hooks.log"), fmt.Sprintf("%s: %q err=%v out=%q PATH=%s\n", event, c, err, out, os.Getenv("PATH")))
			}
		}
	}
	write := func(rec map[string]any) {
		rec["sessionId"] = sid
		b, _ := json.Marshal(rec)
		f, err := os.OpenFile(transcript, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.Write(append(b, '\n'))
	}
	assistant := func(block map[string]any, extra map[string]any) {
		rec := map[string]any{"type": "assistant", "isSidechain": false,
			"message": map[string]any{"role": "assistant", "content": []any{block}}}
		for k, v := range extra {
			rec[k] = v
		}
		write(rec)
	}

	fire("SessionStart", map[string]any{"source": source})
	fmt.Print("\x1b[?2004h> ")

	exit := false
	submit := func(prompt string) {
		if prompt == "/exit" {
			fire("SessionEnd", map[string]any{"reason": "prompt_input_exit"})
			exit = true
			return
		}
		if strings.HasPrefix(prompt, "!") {
			// Measured on the real CLI: shell mode. No UserPromptSubmit, nothing
			// sent to the model — and the command RUNS, as it does there, so a
			// test can see a shell-mode escape by the file it creates.
			write(map[string]any{"type": "user", "isSidechain": false,
				"message": map[string]any{"role": "user", "content": "<bash-input>" + prompt[1:] + "</bash-input>"}})
			_ = exec.Command("sh", "-c", prompt[1:]).Run()
			fmt.Print("\r\n! ran locally\r\n> ")
			return
		}
		if fakeLocalCommands[strings.Fields(prompt)[0]] {
			// A local slash command: no UserPromptSubmit, no model turn. The record
			// keeps the bytes that arrived, so a test can see them.
			write(map[string]any{"type": "user", "isSidechain": false,
				"message": map[string]any{"role": "user", "content": "<local-command>" + prompt + "</local-command>"}})
			fmt.Print("\r\n/ ran locally\r\n> ")
			return
		}
		pid := randomID()
		fire("UserPromptSubmit", map[string]any{"prompt_id": pid, "prompt": prompt, "permission_mode": "default"})
		write(map[string]any{"type": "user", "promptId": pid, "isSidechain": false,
			"message": map[string]any{"role": "user", "content": prompt}})
		if strings.Contains(prompt, "FAIL-AUTH") {
			msg := "Please run /login · API Error: 401 OAuth access token is invalid."
			assistant(map[string]any{"type": "text", "text": msg},
				map[string]any{"isApiErrorMessage": true, "error": "authentication_failed", "apiErrorStatus": 401})
			fire("StopFailure", map[string]any{"prompt_id": pid, "error": "authentication_failed", "last_assistant_message": msg})
			write(map[string]any{"type": "system", "subtype": "turn_duration", "durationMs": 1})
			fmt.Print("\r\n" + msg + "\r\n> ")
			return
		}
		if strings.Contains(prompt, "ASK-PERMISSION") {
			// A tool call that stops at the terminal's permission dialog: the
			// Notification hook fires and NOTHING else until someone answers —
			// no Stop, so the session stays busy.
			fire("Notification", map[string]any{"notification_type": "permission_prompt",
				"message": "Claude needs your permission to use Bash"})
			fmt.Print("\r\nDo you want to proceed?\r\n")
			return
		}
		if strings.Contains(prompt, "SLOW") {
			time.Sleep(1500 * time.Millisecond)
		}
		sum := sha256.Sum256([]byte(prompt))
		first := fmt.Sprintf("received %d bytes", len(prompt))
		if model != "" {
			// A lane's CLI names its model, so a reply proves which CLI answered.
			first += " on " + model
		}
		second := "sha256 " + hex.EncodeToString(sum[:])
		assistant(map[string]any{"type": "thinking", "thinking": "PRIVATE REASONING", "signature": "s"}, nil)
		assistant(map[string]any{"type": "text", "text": first}, nil)
		assistant(map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "true"}}, nil)
		write(map[string]any{"type": "user", "promptId": pid, "isSidechain": false,
			"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": ""}}}})
		assistant(map[string]any{"type": "text", "text": second}, nil)
		fire("Stop", map[string]any{"prompt_id": pid, "stop_hook_active": false, "last_assistant_message": second})
		write(map[string]any{"type": "system", "subtype": "stop_hook_summary"})
		write(map[string]any{"type": "system", "subtype": "turn_duration", "durationMs": 1})
		fmt.Print("\r\n" + first + "\r\n" + second + "\r\n> ")
	}

	pasteStart, pasteEnd := []byte("\x1b[200~"), []byte("\x1b[201~")
	var in, cur []byte
	inPaste := false
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return 0
		}
		in = append(in, buf[:n]...)
		for len(in) > 0 {
			switch {
			case bytes.HasPrefix(in, pasteStart):
				inPaste, in = true, in[len(pasteStart):]
				continue
			case bytes.HasPrefix(in, pasteEnd):
				inPaste, in = false, in[len(pasteEnd):]
				continue
			case in[0] == 0x1b && (bytes.HasPrefix(pasteStart, in) || bytes.HasPrefix(pasteEnd, in)):
				// A marker split across reads: wait for the rest.
			default:
				c := in[0]
				in = in[1:]
				switch {
				case inPaste && c == '\r':
					cur = append(cur, '\n')
				case inPaste:
					cur = append(cur, c)
				case c == '\r':
					if len(cur) > 0 {
						submit(string(cur))
						cur = nil
						if exit {
							return 0
						}
					}
				default:
					cur = append(cur, c)
				}
				continue
			}
			break
		}
	}
}

func appendFile(path, s string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(s)
}

func blockForever() {
	buf := make([]byte, 256)
	for {
		if _, err := os.Stdin.Read(buf); err != nil {
			os.Exit(0)
		}
	}
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// loadHooks reads settings.json's hooks as event -> commands.
func loadHooks(path string) map[string][]string {
	out := map[string][]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if json.Unmarshal(b, &s) != nil {
		return out
	}
	for ev, groups := range s.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if h.Type == "command" {
					out[ev] = append(out[ev], h.Command)
				}
			}
		}
	}
	return out
}
