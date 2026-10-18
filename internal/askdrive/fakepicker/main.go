// Command fakepicker runs askdrive.FakePicker on a terminal, for the tests
// that need a picker in a REAL tmux pane (internal/mirrorhost's tmuxit suite)
// where no Claude Code is. It draws the picker, reads keys from stdin (put the
// tty in raw mode first: `stty raw -echo`), and when the picker closes writes
// the result to the file named by its second argument:
//
//	fakepicker <questions.json> <result.json>
//
// result.json is {"answers": {...}} or {"cancelled": true}. It is a test tool
// and is never built into a shipped binary.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/ZacxDev/muster/internal/askdrive"
	"github.com/ZacxDev/muster/internal/askq"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: fakepicker <questions.json> <result.json>")
		os.Exit(2)
	}
	b, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var qs []askq.Question
	if err := json.Unmarshal(b, &qs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	p := askdrive.NewFakePicker(qs)
	draw := func() {
		// Home, clear, then the screen with CRLF line ends (the tty is raw).
		out := "\x1b[H\x1b[2J"
		for _, l := range splitLines(p.Screen()) {
			out += l + "\r\n"
		}
		_, _ = os.Stdout.WriteString(out)
	}
	draw()
	buf := make([]byte, 256)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			p.Type(string(buf[:n]))
			draw()
			if answers, cancelled, done := p.Result(); done {
				res, _ := json.Marshal(map[string]any{"answers": answers, "cancelled": cancelled})
				_ = os.WriteFile(os.Args[2], res, 0o600)
			}
		}
		if err != nil {
			return
		}
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
