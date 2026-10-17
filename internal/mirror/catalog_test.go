package mirror_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// THE COMPOSER MENU'S CATALOG over the wire (catalog.go): its normaliser and
// bounds, a REAL muster-mirror answering a real Service, a hand-driven host
// that shows exactly which frames muster sends, and an older host that is
// never asked.

// TestNormalizeCatalogKeepsWhatTheWireMayCarry: invalid names, sources, kinds
// and triggers are dropped; a name two sources define is kept once, by
// project > user > plugin > built-in; descriptions and labels lose control
// and bidi characters; a snippet body keeps its newlines and tabs and loses
// the rest; commands come out sorted, snippets in their own order.
func TestNormalizeCatalogKeepsWhatTheWireMayCarry(t *testing.T) {
	c := mirror.NormalizeCatalog(mirror.Catalog{
		Commands: []mirror.CatalogEntry{
			{Name: "zeta", Description: "from the user", Source: mirror.SourceUser, Kind: mirror.KindCommand},
			{Name: "zeta", Description: "from the project", Source: mirror.SourceProject, Kind: mirror.KindCommand},
			{Name: "zeta", Description: "built in", Source: mirror.SourceBuiltin, Kind: mirror.KindCommand},
			{Name: "stripe:test-cards", Description: "a\x1b[31mred\u202eevil\nline", Source: mirror.SourcePlugin, Kind: mirror.KindSkill},
			{Name: "<script>", Source: mirror.SourceUser, Kind: mirror.KindCommand},
			{Name: "-dash", Source: mirror.SourceUser, Kind: mirror.KindCommand},
			{Name: "ok", Source: "elsewhere", Kind: mirror.KindCommand},
			{Name: "ok2", Source: mirror.SourceUser, Kind: "agent"},
			{Name: strings.Repeat("n", mirror.MaxCatalogName+1), Source: mirror.SourceUser, Kind: mirror.KindCommand},
			{Name: "alpha", Description: `<img src=x onerror="alert(1)">`, Source: mirror.SourceUser, Kind: mirror.KindSkill},
		},
		Snippets: []mirror.Snippet{
			{Trigger: ":second", Label: "B", Body: "line one\r\nline\ttwo\x07"},
			{Trigger: ":first", Label: "A", Body: "a"},
			{Trigger: ":second", Label: "dup", Body: "dup"},
			{Trigger: "nocolon", Body: "x"},
			{Trigger: ":blank", Body: " \n\t "},
			{Trigger: ":too-long", Body: strings.Repeat("x", mirror.MaxSnippetBody+1)},
		},
	})
	var got []string
	for _, e := range c.Commands {
		got = append(got, e.Source+"/"+e.Name+"="+e.Description)
	}
	want := []string{
		`user/alpha=<img src=x onerror="alert(1)">`,
		"plugin/stripe:test-cards=a [31mred evil line",
		"project/zeta=from the project",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("commands:\n got %q\nwant %q", got, want)
	}
	if len(c.Snippets) != 2 || c.Snippets[0] != (mirror.Snippet{Trigger: ":second", Label: "B", Body: "line one\nline\ttwo"}) || c.Snippets[1].Trigger != ":first" {
		t.Fatalf("snippets = %+v", c.Snippets)
	}
	if c.Truncated {
		t.Fatal("nothing was over a bound, yet Truncated is set")
	}
	// A fixed point: normalising what was normalised changes nothing.
	again := mirror.NormalizeCatalog(c)
	a, _ := json.Marshal(c)
	b, _ := json.Marshal(again)
	if string(a) != string(b) {
		t.Fatalf("not a fixed point:\n%s\n%s", a, b)
	}
}

// TestTheCatalogCountsAreCapped: past MaxCatalogCommands / MaxCatalogSnippets
// the rest are dropped and Truncated says so.
func TestTheCatalogCountsAreCapped(t *testing.T) {
	var in mirror.Catalog
	for i := 0; i < mirror.MaxCatalogCommands+7; i++ {
		in.Commands = append(in.Commands, mirror.CatalogEntry{Name: "c" + itoa(10000+i), Source: mirror.SourceUser, Kind: mirror.KindCommand})
	}
	for i := 0; i < mirror.MaxCatalogSnippets+3; i++ {
		in.Snippets = append(in.Snippets, mirror.Snippet{Trigger: ":s" + itoa(i), Body: "b"})
	}
	c := mirror.NormalizeCatalog(in)
	if len(c.Commands) != mirror.MaxCatalogCommands || len(c.Snippets) != mirror.MaxCatalogSnippets || !c.Truncated {
		t.Fatalf("%d commands, %d snippets, truncated=%v; want %d, %d, true", len(c.Commands), len(c.Snippets), c.Truncated, mirror.MaxCatalogCommands, mirror.MaxCatalogSnippets)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// TestTheLargestCatalogFitsOneFrame: a host's worst case — every count at its
// cap, every field at its bound and written in the character JSON escapes
// six-fold — is cut to MaxCatalogBytes, and the cataloged MESSAGE carrying it
// stays inside MaxFrame (a message over it ends the whole connection). The
// figure is logged.
func TestTheLargestCatalogFitsOneFrame(t *testing.T) {
	var in mirror.Catalog
	worst := strings.Repeat("<", mirror.MaxCatalogDescription)
	for i := 0; i < mirror.MaxCatalogCommands; i++ {
		name := "c" + strings.Repeat("x", mirror.MaxCatalogName-6) + itoa(10000+i)
		in.Commands = append(in.Commands, mirror.CatalogEntry{Name: name, Description: worst, Source: mirror.SourcePlugin, Kind: mirror.KindSkill})
	}
	for i := 0; i < mirror.MaxCatalogSnippets; i++ {
		in.Snippets = append(in.Snippets, mirror.Snippet{Trigger: ":" + strings.Repeat("t", mirror.MaxSnippetTrigger-6) + itoa(10000+i), Label: worst, Body: strings.Repeat("<", mirror.MaxSnippetBody)})
	}
	c := mirror.NormalizeCatalog(in)
	if !c.Truncated {
		t.Fatal("the worst case was not cut, so the byte budget is not what bounded it")
	}
	if len(c.Snippets) == 0 || len(c.Commands) == 0 {
		t.Fatalf("the worst case kept %d snippets and %d commands: one kind pushed the other out entirely", len(c.Snippets), len(c.Commands))
	}
	body, _ := json.Marshal(c)
	msg, _ := json.Marshal(mirror.Msg{Type: mirror.TypeCataloged, ID: ^uint32(0), Catalog: &c, Error: strings.Repeat("e", 200)})
	t.Logf("the largest catalog: %d commands, %d snippets, %d bytes encoded (budget %d); its message %d bytes = %.1f%% of MaxFrame (%d)",
		len(c.Commands), len(c.Snippets), len(body), mirror.MaxCatalogBytes, len(msg), 100*float64(len(msg))/float64(mirror.MaxFrame), mirror.MaxFrame)
	if len(body) > mirror.MaxCatalogBytes {
		t.Fatalf("the catalog encodes to %d bytes, over MaxCatalogBytes %d", len(body), mirror.MaxCatalogBytes)
	}
	if len(msg) > mirror.MaxFrame {
		t.Fatalf("the message is %d bytes, over MaxFrame %d", len(msg), mirror.MaxFrame)
	}
}

// TestACatalogMessageRoundTrips: Msg.Catalog and Msg.CatalogReport survive
// encoding, and a message from a host that predates them decodes to neither.
func TestACatalogMessageRoundTrips(t *testing.T) {
	in := mirror.Msg{Type: mirror.TypeCataloged, ID: 7, Catalog: &mirror.Catalog{
		Commands: []mirror.CatalogEntry{{Name: "a:b", Description: "d", Source: mirror.SourcePlugin, Kind: mirror.KindSkill}},
		Snippets: []mirror.Snippet{{Trigger: ":x", Label: "l", Body: "b\nc"}}, Truncated: true}}
	b, _ := json.Marshal(in)
	var out mirror.Msg
	if err := json.Unmarshal(b, &out); err != nil || out.Catalog == nil || out.ID != 7 {
		t.Fatalf("round trip: %v %+v", err, out)
	}
	if got, _ := json.Marshal(out); string(got) != string(b) {
		t.Fatalf("round trip changed it:\n%s\n%s", b, got)
	}
	var old mirror.Msg
	_ = json.Unmarshal([]byte(`{"type":"hello","version":2,"host":"h","sessions":["s"]}`), &old)
	if old.CatalogReport || old.Catalog != nil {
		t.Fatalf("an older hello decodes as advertising a catalog: %+v", old)
	}
	h, _ := json.Marshal(mirror.Msg{Type: mirror.TypeHello, CatalogReport: true})
	if !strings.Contains(string(h), `"catalogReport":true`) {
		t.Fatalf("hello encodes as %s", h)
	}
}

// cwdBackend is claudeBackend plus mirrorhost.CwdResolver: every window runs
// in cwd.
type cwdBackend struct {
	*claudeBackend
	cwd string
}

func (b cwdBackend) WindowCwd(context.Context, string, string) (string, error) { return b.cwd, nil }

// catalogFixture is a config dir with one user skill, a project with one
// command, and a snippets file — all distinct names.
func catalogFixture(t *testing.T) (*mirrorhost.CatalogScanner, string) {
	t.Helper()
	root := t.TempDir()
	w := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, proj := filepath.Join(root, "cfg"), filepath.Join(root, "home", "proj")
	w(filepath.Join(cfg, "skills", "wire-skill", "SKILL.md"), "---\nname: wire-skill\ndescription: a user skill over the wire\n---\n")
	w(filepath.Join(proj, ".git", "HEAD"), "x")
	w(filepath.Join(proj, ".claude", "commands", "proj-cmd.md"), "---\ndescription: the window's project command\n---\n")
	snip := filepath.Join(root, "snippets.json")
	w(snip, `[{"trigger":":wire","label":"Over the wire","body":"a prewritten message"}]`)
	return &mirrorhost.CatalogScanner{Config: cfg, Home: filepath.Join(root, "home"), SnippetsFile: snip}, proj
}

// connectCatalog runs a REAL muster-mirror client with scanner s.
func connectCatalog(t *testing.T, r *rig, host, token string, b mirrorhost.Backend, s *mirrorhost.CatalogScanner, noCatalog bool) {
	t.Helper()
	c, err := mirrorhost.New(mirrorhost.Config{URL: r.srv.URL, Token: token, Host: host, Backend: b, Interactive: true,
		AllowHTTP: true, Refresh: 30 * time.Millisecond, Logger: log.New(io.Discard, "", 0), Catalog: s, NoCatalog: noCatalog})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = c.RunOnce(ctx) }()
	t.Cleanup(cancel)
}

// TestARealHostAnswersItsWindowsCatalog is the feature end to end over the
// tunnel: the user's skill, the WINDOW's project command (found from the cwd
// the backend reports) and the snippets — and only the host's owner can ask.
func TestARealHostAnswersItsWindowsCatalog(t *testing.T) {
	r := newRig(t)
	s, proj := catalogFixture(t)
	tok, _ := r.store.mint(t, 1, "cat")
	connectCatalog(t, r, "cat", tok, cwdBackend{newClaudeBackend("main"), proj}, s, false)
	r.online(t, 1, "cat", "main")

	c, err := r.svc.CatalogWindow(asUser(1), "cat", "main", "")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	got := map[string]string{}
	for _, e := range c.Commands {
		got[e.Name] = e.Source + ":" + e.Description
	}
	if got["wire-skill"] != "user:a user skill over the wire" || got["proj-cmd"] != "project:the window's project command" {
		t.Fatalf("commands = %v", got)
	}
	if len(c.Snippets) != 1 || c.Snippets[0].Trigger != ":wire" || c.Snippets[0].Body != "a prewritten message" {
		t.Fatalf("snippets = %+v", c.Snippets)
	}
	// Another owner's view: the host is not there at all.
	if _, err := r.svc.CatalogWindow(asUser(2), "cat", "main", ""); !errors.Is(err, mirror.ErrHostOffline) {
		t.Fatalf("another owner: %v, want ErrHostOffline", err)
	}
	// A session the host does not list.
	if _, err := r.svc.CatalogWindow(asUser(1), "cat", "nope", ""); !errors.Is(err, mirror.ErrSessionGone) {
		t.Fatalf("an unlisted session: %v, want ErrSessionGone", err)
	}
}

// TestAnOlderHostIsNeverAskedForACatalog: a host that does not advertise one
// (a muster-mirror from before it — played by NoCatalog, and by a raw hello
// without the field) is refused with ErrCatalogUnsupported and SENT NOTHING;
// the positive control, the same raw host advertising it, IS sent a request.
func TestAnOlderHostIsNeverAskedForACatalog(t *testing.T) {
	r := newRig(t)
	s, _ := catalogFixture(t)
	tok, _ := r.store.mint(t, 1, "old")
	connectCatalog(t, r, "old", tok, newClaudeBackend("main"), s, true)
	r.online(t, 1, "old", "main")
	if _, err := r.svc.CatalogWindow(asUser(1), "old", "main", ""); !errors.Is(err, mirror.ErrCatalogUnsupported) {
		t.Fatalf("a NoCatalog host: %v, want ErrCatalogUnsupported", err)
	}

	for _, advertise := range []bool{false, true} {
		host := map[bool]string{false: "rawold", true: "rawnew"}[advertise]
		tok, _ := r.store.mint(t, 1, host)
		hl := hello(host, true, "main")
		hl.CatalogReport = advertise
		raw, err := r.rawConnect(t, tok, hl)
		if err != nil {
			t.Fatal(err)
		}
		r.online(t, 1, host, "main")
		ctx, cancel := context.WithTimeout(asUser(1), 300*time.Millisecond)
		_, err = r.svc.CatalogWindow(ctx, host, "main", "")
		cancel()
		asked := false
		raw.mu.Lock()
		for _, m := range raw.text {
			if m.Type == mirror.TypeCatalog {
				asked = true
			}
		}
		raw.mu.Unlock()
		if advertise != asked {
			t.Fatalf("advertise=%v: asked=%v (err %v)", advertise, asked, err)
		}
		if !advertise && !errors.Is(err, mirror.ErrCatalogUnsupported) {
			t.Fatalf("an older raw host: %v, want ErrCatalogUnsupported", err)
		}
	}
}

// TestWhatAHostAnswersIsNormalisedAndItsRefusalNamed: muster applies
// NormalizeCatalog to a host's answer itself (a host that sends an invalid
// name and a control character does not get them shown), a host's refusal
// comes back as ErrCatalogRefused, and a host that drops mid-request ends it
// with ErrHostDisconnected.
func TestWhatAHostAnswersIsNormalisedAndItsRefusalNamed(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "raw")
	hl := hello("raw", true, "main")
	hl.CatalogReport = true
	raw, err := r.rawConnect(t, tok, hl)
	if err != nil {
		t.Fatal(err)
	}
	r.online(t, 1, "raw", "main")
	// The raw host answers each request as it is told to.
	answers := make(chan func(id uint32) mirror.Msg, 4)
	go func() {
		seen := map[uint32]bool{}
		for {
			raw.mu.Lock()
			var req *mirror.Msg
			for i := range raw.text {
				if m := raw.text[i]; m.Type == mirror.TypeCatalog && !seen[m.ID] {
					seen[m.ID] = true
					req = &m
					break
				}
			}
			raw.mu.Unlock()
			if req == nil {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			f, ok := <-answers
			if !ok {
				return
			}
			if f == nil {
				_ = raw.ws.Close(websocket.StatusNormalClosure, "gone")
				return
			}
			b, _ := json.Marshal(f(req.ID))
			_ = raw.ws.Write(context.Background(), websocket.MessageText, b)
		}
	}()

	answers <- func(id uint32) mirror.Msg {
		return mirror.Msg{Type: mirror.TypeCataloged, ID: id, Catalog: &mirror.Catalog{Commands: []mirror.CatalogEntry{
			{Name: "good", Description: "fine\x00\u202ehidden", Source: mirror.SourceUser, Kind: mirror.KindCommand},
			{Name: "bad name", Source: mirror.SourceUser, Kind: mirror.KindCommand},
		}}}
	}
	c, err := r.svc.CatalogWindow(asUser(1), "raw", "main", "")
	if err != nil || len(c.Commands) != 1 || c.Commands[0].Name != "good" || c.Commands[0].Description != "fine hidden" {
		t.Fatalf("normalised answer: %v %+v", err, c)
	}
	answers <- func(id uint32) mirror.Msg { return mirror.Msg{Type: mirror.TypeCataloged, ID: id, Error: "nope"} }
	if _, err := r.svc.CatalogWindow(asUser(1), "raw", "main", ""); !errors.Is(err, mirror.ErrCatalogRefused) || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("a refusal: %v, want ErrCatalogRefused naming the host's reason", err)
	}
	answers <- nil
	if _, err := r.svc.CatalogWindow(asUser(1), "raw", "main", ""); !errors.Is(err, mirror.ErrHostDisconnected) {
		t.Fatalf("a host that dropped: %v, want ErrHostDisconnected", err)
	}
}
