package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Transcript bodies shaped like the pinned CLI's (measured on 2.1.289 and on
// workstation CLIs): header entries that carry no "entrypoint" come first, then
// the conversation's entries, each stamped with how the CLI was started —
// "cli" for an interactive session, "sdk-cli" for `claude -p`.
const (
	cliTranscript = `{"type":"mode","mode":"default"}` + "\n" +
		`{"type":"permission-mode","permissionMode":"default"}` + "\n" +
		`{"type":"user","entrypoint":"cli","message":{"role":"user","content":"hello"}}` + "\n"
	sdkTranscript = `{"type":"queue-operation","operation":"enqueue"}` + "\n" +
		`{"type":"attachment","entrypoint":"sdk-cli"}` + "\n" +
		`{"type":"user","entrypoint":"sdk-cli","message":{"role":"user","content":"hello"}}` + "\n"
)

// writeTranscriptAt writes body as id's transcript in dir, modified at at.
func writeTranscriptAt(t *testing.T, dir, id, body string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

// 🟡 A `claude -p` RUN IN THE POD IS NOT THE PRIMARY'S CONVERSATION. With no
// record, the primary resumes the newest transcript in its project directory
// that is not a lane's — and `claude -p` (a chief's shell, a script) writes a
// top-level transcript there too, stamped "entrypoint":"sdk-cli". It must be
// skipped for the older interactive ("cli") one, however new it is.
func TestTheUnrecordedFallbackSkipsAnSDKRun(t *testing.T) {
	cfg, ws := t.TempDir(), "/data/workspace"
	dir := projectDir(cfg, ws)
	sup := newSupervisor(nil, "claude", cfg, ws)
	const primary = "0f0e0d0c-0b0a-4908-8706-050403020100"
	const sdk = "1f0e0d0c-0b0a-4908-8706-050403020100"
	now := time.Now()
	writeTranscriptAt(t, dir, primary, cliTranscript, now.Add(-time.Hour))
	// Control: alone, the interactive transcript is resumed.
	if argv, _ := sup.argv(); strings.Join(argv, " ") != "claude --resume "+primary {
		t.Fatalf("control: one cli transcript: %q; want --resume %s", argv, primary)
	}
	writeTranscriptAt(t, dir, sdk, sdkTranscript, now)
	if argv, mode := sup.argv(); strings.Join(argv, " ") != "claude --resume "+primary || mode != modeContinue {
		t.Fatalf("A `claude -p` RUN TAKEN AS THE PRIMARY: a newer sdk-cli transcript beside an older cli one: %q %s; "+
			"want --resume %s (the interactive one)", argv, mode, primary)
	}
}

// realHeader is what precedes the first entry carrying an entrypoint in a real
// interactive transcript, shaped like the pinned CLI's: a long last-prompt, the
// mode entries, and a file-history snapshot — several KiB in all (past the
// first KiB in most of 811 workstation transcripts measured).
var realHeader = func() string {
	var b strings.Builder
	b.WriteString(`{"type":"last-prompt","lastPrompt":"` + strings.Repeat("please refactor the lane gate and keep the tests green ", 30) + `","sessionId":"0f0e0d0c-0b0a-4908-8706-050403020100"}` + "\n")
	b.WriteString(`{"type":"mode","mode":"default","sessionId":"0f0e0d0c-0b0a-4908-8706-050403020100"}` + "\n")
	b.WriteString(`{"type":"permission-mode","permissionMode":"default","sessionId":"0f0e0d0c-0b0a-4908-8706-050403020100"}` + "\n")
	b.WriteString(`{"type":"atis-latch","sessionId":"0f0e0d0c-0b0a-4908-8706-050403020100"}` + "\n")
	b.WriteString(`{"type":"file-history-snapshot","messageId":"m1","snapshot":{"trackedFileBackups":{`)
	for i := 0; i < 40; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"/data/workspace/cmd/ccd/file_` + strings.Repeat("x", 20) + `_` + string(rune('a'+i%26)) + `.go":{"backupFileName":"abcdef0123456789@v2","version":2,"backupTime":"2000-01-01T00:00:00.000Z"}`)
	}
	b.WriteString(`}},"isSnapshotUpdate":false}` + "\n")
	return b.String()
}()

// The fallback chain, tier by tier, with no record: (1) the newest transcript
// stamped "cli"; (2) else the newest non-lane transcript of ANY stamp — a CLI
// that renames, drops or moves the stamp degrades to the old "newest non-lane"
// choice, never to a fresh start that abandons the conversation; (3) else a
// fresh start. Each choice says which tier made it in the log.
func TestTheUnrecordedFallbackTiers(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	cfg, ws := t.TempDir(), "/data/workspace"
	dir := projectDir(cfg, ws)
	sup := newSupervisor(nil, "claude", cfg, ws)
	const (
		stamped   = "0f0e0d0c-0b0a-4908-8706-050403020100"
		sdk       = "1f0e0d0c-0b0a-4908-8706-050403020100"
		unstamped = "2f0e0d0c-0b0a-4908-8706-050403020100"
	)
	now := time.Now()
	argvLog := func() (string, string) {
		t.Helper()
		logBuf.Reset()
		argv, _ := sup.argv()
		return strings.Join(argv, " "), strings.TrimSpace(logBuf.String())
	}
	check := func(what, wantArgv, wantLog string) {
		t.Helper()
		argv, logged := argvLog()
		if argv != wantArgv || !strings.HasSuffix(logged, wantLog) {
			t.Errorf("%s: argv %q, log %q; want argv %q, log ending %q", what, argv, logged, wantArgv, wantLog)
		}
	}

	check("tier 3, nothing on disk", "claude", "ccd: starting fresh: no primary transcript")

	// Only a transcript with no stamp (a CLI that dropped the field), and a lane's.
	writeTranscriptAt(t, dir, laneUUID("k"), cliTranscript, now.Add(time.Hour))
	writeTranscriptAt(t, dir, unstamped, `{"type":"mode"}`+"\n"+`{"type":"user","message":{"content":"hi"}}`+"\n", now.Add(-2*time.Hour))
	check("tier 2, only an unstamped transcript", "claude --resume "+unstamped,
		"ccd: resuming "+unstamped+": newest primary transcript (no record, unstamped fallback)")

	// A newer sdk-cli one: tier 2 is the newest of ANY stamp.
	writeTranscriptAt(t, dir, sdk, sdkTranscript, now)
	check("tier 2, newest of any stamp", "claude --resume "+sdk,
		"ccd: resuming "+sdk+": newest primary transcript (no record, unstamped fallback)")

	// An OLDER cli-stamped one, behind real-shaped KiB of header: tier 1 wins.
	writeTranscriptAt(t, dir, stamped, realHeader+cliTranscript, now.Add(-time.Hour))
	check("tier 1, stamped cli beats newer others", "claude --resume "+stamped,
		"ccd: resuming "+stamped+": newest primary transcript (no record, stamped cli)")
}

// TestTheEntrypointIsReadStructurally: the decision is the FIRST entry that
// carries an entrypoint, read as JSON — not a substring anywhere in the head,
// not a field at a fixed position.
func TestTheEntrypointIsReadStructurally(t *testing.T) {
	cases := []struct {
		name, body string
		want       bool
	}{
		{"interactive", cliTranscript, true},
		// Several KiB of real-shaped header before the first stamped entry.
		{"interactive behind a real-sized header", realHeader + cliTranscript, true},
		{"claude -p", sdkTranscript, false},
		{"entrypoint before type", `{"entrypoint":"cli","type":"user"}` + "\n", true},
		{"another entrypoint", `{"type":"user","entrypoint":"sdk-ts"}` + "\n", false},
		{"no entrypoint at all", `{"type":"mode"}` + "\n" + `{"type":"user"}` + "\n", false},
		{"empty", "", false},
		// "cli" as a VALUE elsewhere (a prompt's text) is not the entrypoint.
		{"cli only in content", `{"type":"user","entrypoint":"sdk-cli","message":{"content":"\"entrypoint\":\"cli\""}}` + "\n", false},
		{"cli only in a nested field", `{"type":"user","entrypoint":"sdk-cli","meta":{"entrypoint":"cli"}}` + "\n", false},
		// The first entry that has one decides; a later one does not.
		{"first decides (sdk then cli)", `{"entrypoint":"sdk-cli"}` + "\n" + `{"entrypoint":"cli"}` + "\n", false},
		{"first decides (cli then sdk)", `{"entrypoint":"cli"}` + "\n" + `{"entrypoint":"sdk-cli"}` + "\n", true},
		{"a garbled line is skipped", "not json\n" + `{"entrypoint":"cli"}` + "\n", true},
		// A transcript whose head is bigger than the cap is not read past it.
		{"past the cap", `{"type":"x","pad":"` + strings.Repeat("a", transcriptHeadCap) + `"}` + "\n" + `{"entrypoint":"cli"}` + "\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "t.jsonl")
			if err := os.WriteFile(p, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := interactiveTranscript(p); got != c.want {
				t.Fatalf("interactiveTranscript = %v, want %v", got, c.want)
			}
		})
	}
}

// 🟡 A LANE'S /clear IS NOT THE PRIMARY'S CONVERSATION. /clear in a lane's CLI
// starts a session whose id is not marked as a lane's (laneUUIDMarker), and its
// transcript is interactive ("cli") like the primary's — so neither the name
// nor the entrypoint tells it apart. Its SessionStart arrives on the lane's
// hook path; that id is recorded, and the unrecorded fallback skips it.
func TestTheUnrecordedFallbackSkipsALanesClearedSession(t *testing.T) {
	cfg, ws := t.TempDir(), "/data/workspace"
	primary := newServer(serverConfig{ConfigDir: cfg}, nil, newAuthTracker())
	primary.recordsPrimary = true
	primary.primaryPID = func(context.Context) (int, error) { return 0, os.ErrNotExist } // the primary's id goes unrecorded
	laneSrv := newServer(serverConfig{ConfigDir: cfg}, nil, newAuthTracker())
	id := laneID("k")
	primary.lanes = &lanes{set: map[string]*lane{id: {id: id, key: "k", srv: laneSrv}}}
	hs := httptest.NewServer(primary.hookHandler())
	t.Cleanup(hs.Close)
	post := func(q, body string) {
		t.Helper()
		resp, err := http.Post(hs.URL+"/hook/SessionStart"+q, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	const own = "0f0e0d0c-0b0a-4908-8706-050403020100"
	const cleared = "11111111-2222-4333-8444-555555555555"
	dir := projectDir(cfg, ws)
	now := time.Now()
	writeTranscriptAt(t, dir, own, cliTranscript, now.Add(-time.Hour))
	post("?pid=31", `{"hook_event_name":"SessionStart","session_id":"`+own+`"}`)
	post("?lane="+id, `{"hook_event_name":"SessionStart","session_id":"`+laneUUID("k")+`"}`)
	post("?lane="+id, `{"hook_event_name":"SessionStart","session_id":"`+cleared+`"}`)
	writeTranscriptAt(t, dir, cleared, cliTranscript, now)
	if _, err := os.Stat(filepath.Join(cfg, primaryRecordFile)); err == nil {
		t.Fatal("control: the primary's id must be unrecorded here")
	}
	sup := newSupervisor(nil, "claude", cfg, ws)
	if argv, _ := sup.argv(); strings.Join(argv, " ") != "claude --resume "+own {
		t.Fatalf("A LANE'S /clear TAKEN AS THE PRIMARY: %q; want --resume %s (the lane's cleared session is %s)", argv, own, cleared)
	}
	// Control: the same transcript, never seen on a lane's hook path, is resumed.
	if err := os.Remove(filepath.Join(cfg, laneSessionsFile)); err != nil {
		t.Fatal(err)
	}
	if argv, _ := sup.argv(); strings.Join(argv, " ") != "claude --resume "+cleared {
		t.Fatalf("control: with no lane record the newer cli transcript is resumed: %q", argv)
	}
}

// 🟢 A lane that `/resume`s the PRIMARY's conversation names the primary's id
// on its SessionStart; that id is not recorded as a lane's, or the primary's
// own fallback would pass over it. Control: another id on the same path is.
func TestTheLaneSessionRecordNeverHoldsThePrimarysID(t *testing.T) {
	cfg := t.TempDir()
	const own = "0f0e0d0c-0b0a-4908-8706-050403020100"
	const other = "11111111-2222-4333-8444-555555555555"
	if err := recordPrimary(cfg, own); err != nil {
		t.Fatal(err)
	}
	if err := recordLaneSession(cfg, own); err != nil {
		t.Fatal(err)
	}
	if err := recordLaneSession(cfg, other); err != nil {
		t.Fatal(err)
	}
	got := laneSessions(cfg)
	if got[own] || !got[other] || len(got) != 1 {
		t.Fatalf("lane sessions = %v; want only %s (never the primary's recorded %s)", got, other, own)
	}
}
