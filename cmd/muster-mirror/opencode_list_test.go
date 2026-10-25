package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// ocListBackend lists the windows it holds.
type ocListBackend struct {
	listBackend
	windows []mirrorhost.WindowInfo
}

func (b ocListBackend) Windows(context.Context) ([]mirrorhost.WindowInfo, error) {
	return b.windows, nil
}

// TestListOpenCodeSaysWhatEachWindowShowsAndWhy: `-list -opencode` names the
// token by fingerprint (never the token), whether the server accepts it, and
// per opencode window its pane title, the session the title matched ("matched
// by title") or why it has none; a refused token says so and what fixes it.
func TestListOpenCodeSaysWhatEachWindowShowsAndWhy(t *testing.T) {
	srv := opencodetest.New(pairFixturePassword)
	defer srv.Close()
	srv.PutSession("ses_fixtureA", "Fix the flaky fixture", "")
	srv.PutSession("ses_fixtureB", "New session", "")
	srv.PutSession("ses_fixtureC", "New session", "")
	tok := srv.Issue()
	creds, err := opencodehost.EnvCredentials(srv.URL, tok)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := mirrorhost.NewOpenCodeSource(ctx, creds, "", log.New(io.Discard, "", 0))
	pane := func(id, title string) mirrorhost.WindowInfo {
		ap := mirrorhost.ResolveOpenCodePane(src, id, 42, title)
		return mirrorhost.WindowInfo{Session: "s", ID: "@" + id[1:], Pane: id, Command: "opencode", Agent: &ap}
	}
	b := ocListBackend{windows: []mirrorhost.WindowInfo{pane("%1", "OC | Fix the flaky fixture"), pane("%2", "OC | New session")}}
	var out, errb bytes.Buffer
	if rc := printOpenCode(ctx, b, creds, "", &out, &errb); rc != 0 {
		t.Fatalf("rc %d: %s", rc, errb.String())
	}
	got := out.String()
	for _, want := range []string{
		"token\t-\t" + opencodehost.Fingerprint(tok) + "\texpires=",
		"server\t" + srv.URL + "\tok\tversion=2.0.0-fixture",
		"window\ts\t@1\tpane=%1\ttui=42\ttty=-\tsession=ses_fixtureA\t",
		"\tsource=title\n  pane-title\t\"OC | Fix the flaky fixture\"\n  match\tses_fixtureA\tmatched by title\n",
		"window\ts\t@2\tpane=%2\ttui=42\ttty=-\tsession=-\tstatus=unknown\tsource=process\n",
		"  no-view\t2 opencode sessions match the title \"New session\": ambiguous, so none is shown\n",
		"opencode-windows\t2\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("-list -opencode lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, tok) {
		t.Fatalf("-list -opencode prints the token:\n%s", got)
	}

	// The token refused: said, with the fix; and the windows have no view.
	srv.Rotate("rotated-fixture-password")
	out.Reset()
	src2 := mirrorhost.NewOpenCodeSource(ctx, creds, "", log.New(io.Discard, "", 0))
	ap := mirrorhost.ResolveOpenCodePane(src2, "%1", 42, "OC | Fix the flaky fixture")
	b2 := ocListBackend{windows: []mirrorhost.WindowInfo{{Session: "s", ID: "@1", Pane: "%1", Agent: &ap}}}
	if rc := printOpenCode(ctx, b2, creds, "", &out, &errb); rc != 0 {
		t.Fatal(rc)
	}
	if got := out.String(); !strings.Contains(got, "REFUSED the token") || !strings.Contains(got, "session=-") || !strings.Contains(got, "opencode-pair") {
		t.Fatalf("a refused token:\n%s", got)
	}
	// A token within 3 days of expiry: the same line the view shows.
	out.Reset()
	soon, _ := opencodehost.EnvCredentials(srv.URL, srv.IssueAt(time.Now().Add(2*24*time.Hour+time.Hour)))
	if rc := printOpenCode(ctx, ocListBackend{}, soon, "", &out, &errb); rc != 0 ||
		!strings.Contains(out.String(), "token-health\topencode token expires in 2 days and has not been renewed\n") {
		t.Fatalf("a token about to expire: %q", out.String())
	}
	// No token at all.
	out.Reset()
	if rc := printOpenCode(ctx, ocListBackend{}, nil, "no opencode token at the fixture path", &out, &errb); rc != 0 ||
		!strings.Contains(out.String(), "token\tnone\tno opencode token at the fixture path") {
		t.Fatalf("no token: %q", out.String())
	}
}
