package mirror_test

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// THE RENDERED VIEW'S TRANSCRIPT IS REDACTED ON THE HOST. A REAL muster-mirror
// streams a real transcript file to a hand-driven muster, which keeps every
// text frame the host sends for the view EXACTLY as it came off the socket:
// the backfill, the prompt index, and a live line appended after. A
// token-like secret written into each kind of row — a prompt, a reply, a
// tool call's command, an Agent call's description, an AskUserQuestion's
// question, a tool result — is in none of those bytes, and «redacted» is.

// secrets are the synthetic tokens the rows carry, each in its own row so a
// failure names the field it leaked through.
var secrets = map[string]string{
	"prompt":          "ghp_PromptSecret0001",
	"reply":           "ghp_ReplySecret0002",
	"command":         "ghp_CommandSecret0003",
	"agent call":      "ghp_AgentDescSecret0004",
	"question":        "ghp_QuestionSecret0005",
	"result":          "ghp_ResultSecret0006",
	"live prompt":     "ghp_LivePrompt0007",
	"live result":     "ghp_LiveResult0008",
	"bearer (header)": "abc123LiveBearer",
}

// watchFrames runs a real host against a hand-driven muster that asks it to
// watch "main" and returns a func that waits until a frame of that stream
// contains until, and returns every frame of it so far.
func watchFrames(t *testing.T, b mirrorhost.Backend) func(until string) []string {
	t.Helper()
	frames := make(chan string, 256)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ws.SetReadLimit(mirror.MaxFrame)
		defer ws.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if _, _, err := ws.Read(ctx); err != nil {
			return
		}
		write := func(m mirror.Msg) { bb, _ := json.Marshal(m); _ = ws.Write(ctx, websocket.MessageText, bb) }
		write(mirror.Msg{Type: mirror.TypeWelcome, Version: 2, Host: "laptop"})
		write(mirror.Msg{Type: mirror.TypeWatch, ID: 9, Session: "main"})
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var m mirror.Msg
			if typ == websocket.MessageText && json.Unmarshal(data, &m) == nil && m.ID == 9 {
				frames <- string(data)
			}
		}
	}))
	t.Cleanup(srv.Close)
	c, err := mirrorhost.New(mirrorhost.Config{URL: srv.URL, Token: mirror.TokenPrefix + "x", Host: "laptop",
		Backend: b, AllowHTTP: true, ViewPoll: 20 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	var got []string
	return func(until string) []string {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			for _, f := range got {
				if strings.Contains(f, until) {
					return got
				}
			}
			select {
			case f := <-frames:
				got = append(got, f)
			case <-deadline:
				t.Fatalf("no frame of the watch contained %q; got %d frame(s)", until, len(got))
				return nil
			}
		}
	}
}

func TestTheRenderedViewNeverSendsASecretFromTheTranscript(t *testing.T) {
	ask := `{"questions":[{"question":"Push with GITHUB_TOKEN=` + secrets["question"] + `?","header":"Push","options":[{"label":"Yes","description":"now"},{"label":"No","description":"later"}]}]}`
	tr := newTranscript(t,
		promptLine("deploy it with "+secrets["prompt"]+" please"),
		textLine("Using the token "+secrets["reply"]+" to push."),
		toolLine("tu1", "Bash", `{"command":"git push https://x-access-token:`+secrets["command"]+`@example.test/r.git"}`),
		resultLine("tu1", "remote: token "+secrets["result"]+" accepted", false),
		toolLine("tu2", "Agent", `{"description":"check `+secrets["agent call"]+`","prompt":"go","subagent_type":"general-purpose"}`),
		toolLine("tu3", "AskUserQuestion", ask),
		textLine("BACKFILL-END"),
	)
	b := newClaudeBackend("main")
	b.setPane("main", mirrorhost.ClaudePane{Pane: "%5", Transcript: tr.path, Gen: "g"})
	frames := watchFrames(t, b)

	// The backfill and the prompt index (which lists the backfilled prompt).
	frames(`"promptIndex":true`)
	// A live turn, after the backfill.
	tr.write(promptLine("again with "+secrets["live prompt"]) +
		toolLine("tu4", "Bash", `{"command":"curl -H 'Authorization: Bearer `+secrets["bearer (header)"]+`' https://example.test"}`) +
		resultLine("tu4", "ok "+secrets["live result"], false) + textLine("LIVE-END"))
	all := strings.Join(frames("LIVE-END"), "\n")

	for field, s := range secrets {
		if strings.Contains(all, s) {
			t.Errorf("the %s's secret %q reached the wire", field, s)
		}
	}
	// CONTROL: the frames are the transcript's — the rows are there, redacted.
	for _, want := range []string{"BACKFILL-END", "LIVE-END", "deploy it with «redacted» please", `"promptIndex":true`, "Authorization: «redacted»"} {
		if !strings.Contains(all, want) {
			t.Errorf("the frames do not carry %q:\n%s", want, all)
		}
	}
}
