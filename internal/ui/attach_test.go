package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/uploads"
)

// File attachments (attach.go) in the composer and in transcripts.

// attachSurfaces renders the three composers that carry a paperclip with
// upload path up.
func attachSurfaces(t *testing.T, up string) map[string]string {
	t.Helper()
	return map[string]string{
		"mirror rendered": renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Interactive: true,
			StepUp: stepUpFixture(), Nonce: "0123456789abcdef", Upload: up})),
		"mirror terminal": renderString(t, AgentTerminalPage(AgentTerminalView{Name: "main", StepUp: stepUpFixture(),
			Composer: &ComposerView{SendPath: MirrorTerminalSendPath, Host: "host-a", Session: "main", Upload: up}})),
		"agent terminal": renderString(t, AgentTerminalPage(AgentTerminalView{Name: "keen-wren",
			Composer: &ComposerView{SendPath: AgentSendPath("keen-wren"), Upload: up}})),
	}
}

// TestTheComposerHasAPaperclipOnlyWhenUploadsAreOn: on, each composer draws
// the button (posting to /uploads), the hidden file input, the chips' box and
// the script; off — or a disabled composer — draws none of them.
func TestTheComposerHasAPaperclipOnlyWhenUploadsAreOn(t *testing.T) {
	for name, page := range attachSurfaces(t, UploadPath) {
		for _, want := range []string{`id="mv-attach"`, `data-upload="/uploads"`, `id="mv-file"`, `id="mv-attachments"`, `window.musterAttach = {`} {
			if c := strings.Count(page, want); c != 1 {
				t.Errorf("%s: %q appears %d times; want 1", name, want, c)
			}
		}
		// The file input is NOT a form field the browser would submit.
		if strings.Contains(page, `id="mv-file" name=`) {
			t.Errorf("%s: the file input has a name, so a native submit would carry it", name)
		}
	}
	for name, page := range attachSurfaces(t, "") {
		for _, banned := range []string{`id="mv-attach"`, `id="mv-file"`, `id="mv-attachments"`, `window.musterAttach = {`} {
			if strings.Contains(page, banned) {
				t.Errorf("%s with uploads OFF draws %q", name, banned)
			}
		}
	}
	off := renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Interactive: false, Upload: UploadPath}))
	if strings.Contains(off, `id="mv-attach"`) {
		t.Error("a read-only (disabled) composer draws the paperclip")
	}
}

// TestTheComposerScriptSendsTheAttachmentLines: the send script composes the
// message through window.musterAttach on all three of its paths — the fetch
// body, the browser's own submit, and the empty-text check.
func TestTheComposerScriptSendsTheAttachmentLines(t *testing.T) {
	js := renderString(t, mirrorComposerScript())
	for _, want := range []string{
		"window.musterAttach.compose(el.value)",                  // body()
		"input.value = window.musterAttach.compose(input.value)", // native()
		"att.pending()",              // a Send while uploading waits
		"!(att && att.count())",      // attachments alone are a message
		"window.musterAttach.sent()", // cleared after a send
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the composer script lacks %q", want)
		}
	}
}

func signedLine(t *testing.T, name, ct string, size int64, id string) string {
	t.Helper()
	s, err := uploads.NewSigner("ui-attach-test-key-0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	return uploads.AgentLine(name, ct, size, s.URL("https://muster.example.test", id, time.Unix(1_900_000_000, 0)))
}

const (
	imgID  = "IIIIIIIIIIIIIIIIIIIIII"
	fileID = "FFFFFFFFFFFFFFFFFFFFFF"
)

// TestATranscriptShowsAChipNotTheSignedURL: a prompt carrying two attachment
// lines renders its text as markdown and each line as a chip — the image with
// a thumbnail from the OWNER's route, the other with an icon — and the signed
// link appears nowhere in the HTML.
func TestATranscriptShowsAChipNotTheSignedURL(t *testing.T) {
	msg := "please look at **these**\n" + signedLine(t, "shot.png", "image/png", 319_488, imgID) + "\n" +
		signedLine(t, "notes.txt", "text/plain; charset=utf-8", 120, fileID) + "\nthanks"
	html := renderString(t, promptBody(msg))
	for _, banned := range []string{"sig=", "exp=", "/f/" + imgID, "curl", "[attached:"} {
		if strings.Contains(html, banned) {
			t.Errorf("the rendered prompt carries %q:\n%s", banned, html)
		}
	}
	if c := strings.Count(html, `data-attachment-chip`); c != 2 {
		t.Errorf("%d chips; want 2:\n%s", c, html)
	}
	if !strings.Contains(html, `<img src="/uploads/`+imgID+`/file"`) {
		t.Errorf("no owner-route thumbnail for the image:\n%s", html)
	}
	if strings.Contains(html, `<img src="/uploads/`+fileID+`/file"`) {
		t.Error("a thumbnail was drawn for a text file")
	}
	for _, want := range []string{"<strong>these</strong>", "thanks", "shot.png", "image/png · 312 KB", "notes.txt",
		`data-upload-revoke="/uploads/` + imgID + `/revoke"`} {
		if !strings.Contains(html, want) {
			t.Errorf("the rendered prompt lacks %q:\n%s", want, html)
		}
	}
	// Plain text is unchanged: no chip, the markdown it always was.
	if plain := renderString(t, promptBody("just **text**")); plain != renderString(t, renderMarkdown("just **text**")) {
		t.Errorf("a prompt with no attachment renders differently: %s", plain)
	}
}

// TestBothTranscriptsDrawTheChip: the mirrored session's rendered view (a
// prompt op) and an agent's chat bubble both route a user message through
// promptBody.
func TestBothTranscriptsDrawTheChip(t *testing.T) {
	msg := "see\n" + signedLine(t, "shot.png", "image/png", 2048, imgID)
	var tr MirrorTranscript
	ops := tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindPrompt, Text: msg, Line: 3}})
	if len(ops) != 1 || !strings.Contains(ops[0].HTML, "data-attachment-chip") || strings.Contains(ops[0].HTML, "sig=") {
		t.Errorf("the mirrored prompt op = %+v", ops)
	}
	user := renderString(t, chatBubble("user", msg, 7))
	if !strings.Contains(user, "data-attachment-chip") || strings.Contains(user, "sig=") || strings.Contains(user, "data-md=") {
		t.Errorf("the agent chat's user bubble: %s", user)
	}
	if !strings.Contains(user, `data-msg-id="7"`) {
		t.Errorf("the bubble lost its message id: %s", user)
	}
	// An assistant quoting a line is not the user's attachment: drawn as it was.
	asst := renderString(t, chatBubble("assistant", msg, 8))
	if !strings.Contains(asst, "data-md") || strings.Contains(asst, "data-attachment-chip") {
		t.Errorf("an assistant bubble: %s", asst)
	}
	var b bytes.Buffer
	_ = chatBubble("user", "no attachment", 9).Render(&b)
	if !strings.Contains(b.String(), "data-md") {
		t.Errorf("a plain user bubble lost its client-side markdown: %s", b.String())
	}
}
