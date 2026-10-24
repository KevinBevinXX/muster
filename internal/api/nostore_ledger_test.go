package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE no-store LEDGER. Back and Forward may restore exactly four documents
// from a cache — an agent's chat page, the mirrored Rendered view, a mirrored
// window's terminal and an agent's terminal (viewDocument) — and NOTHING that
// shows or takes a secret: the sign-in pages, step-up confirms, the account,
// token, credential, passkey, plugin, flow and admin pages (the pages that
// load no telemetry, ui.NavDocumentView.Secret), connectors, devices, and every
// response that carries a minted value. Those keep `Cache-Control: no-store`.
//
// Two halves, because either alone has a hole:
//
//   - STRUCTURAL: every function in this package that marks a response
//     no-store — a call to noStore, formDocument or writeDocumentHeaders, or a
//     "no-store" literal — is named below with what it serves, and viewDocument
//     has exactly its four callers. A writer that stops saying no-store leaves
//     the set and the test fails; so does a new one nobody looked at, and a
//     fifth page made cacheable.
//   - BEHAVIOURAL: the secret pages are requested through the whole handler
//     chain and must answer exactly no-store, and the four views exactly
//     viewDocumentCacheControl — so a helper that keeps its name and loses its
//     header (formDocument without noStore, a later Set overriding it) is red
//     too.

// noStoreWriters is every function that marks a response no-store through a
// helper (noStore, formDocument, writeDocumentHeaders), with what it serves.
var noStoreWriters = map[string]string{
	"writeAuthDocument":   "the bare pages: sign-in, claim, invite, reset, notices",
	"writeNavDocument":    "every signed-in server-written page: account, tokens, credentials, passkeys, plugins, flows, admin, Asks",
	"formDocument":        "the helper the no-script confirm pages share",
	"mirrorStepUpPage":    "the Rendered view's no-script step-up confirm, carrying the message",
	"sendQueueStepUpPage": "the send queue's no-script step-up confirm, carrying the message",
	"sendQueueRetry":      "the send queue's Retry confirm page",
	"sendQueueEdit":       "the send queue's edit form, the queued message in it",
	"handleSendQueueList": "the send queue: the queued messages",
}

// noStoreLiterals is every function holding a "no-store" literal of its own.
var noStoreLiterals = map[string]string{
	"noStore":                  "the helper itself",
	"writeDocumentHeaders":     "the header set of both document writers",
	"handleAccountDevicesLive": "the paired devices, live",
	"handleDeviceConfig":       "a paired phone's config",
	"handleDevicePair":         "a pairing, with its one-time secret",
	"handleDevicePairCode":     "a pairing code",
	"handlePushKey":            "the VAPID public key",
	"renderConnectorsContent":  "the connectors list",
	"handleAgentChatLog":       "the chat-log partial",
	"handleAsksDrafts":         "an ask's draft answers",
	"handleAttentionAsker":     "a page's open asks",
	"handleAttentionBadge":     "the Asks badge",
	"handleAttentionItems":     "the Asks list",
	"handleAttentionOpen":      "an ask opened from a push",
	"writeAskLive":             "an ask's live answer",
	"handleFocusHistory":       "an agent's focus history",
	"handleMirrorAsks":         "a window's open questions",
	"handleMirrorComplete":     "the composer's completions",
	"handleMirrorSubagent":     "a subagent's steps",
	"handleMirrorSuggest":      "a window's suggested replies",
	"handleReviewCount":        "the review count",
	"handleShare":              "the share target",
	"handleTranscribe":         "a dictation's transcript",
	"handleUpload":             "an upload's answer",
	"handleUploadLink":         "an upload's signed link",
}

// viewDocumentCallers are the four handlers whose documents Back may restore.
var viewDocumentCallers = []string{
	"handleAgentDetail",        // /agents/{name}
	"handleAgentTerminalPage",  // /agents/{name}/terminal
	"handleMirrorTerminalPage", // /mirror/terminal
	"handleMirrorViewPage",     // /mirror/view
}

// TestTheNoStoreLedger is the structural half (see the top of this file).
func TestTheNoStoreLedger(t *testing.T) {
	writers, literals, views := noStoreScan(t, ".")
	// POSITIVE CONTROL: a scan wired to nothing would find nothing.
	if len(writers) < 5 || len(literals) < 5 || len(views) == 0 {
		t.Fatalf("the source scan found %d writers, %d literals, %d view callers — it is not observing this package", len(writers), len(literals), len(views))
	}
	if got, want := strings.Join(writers, ","), strings.Join(sortedKeys(noStoreWriters), ","); got != want {
		t.Errorf("the functions marking a response no-store through a helper are\n  %s\nthe ledger names\n  %s\n"+
			"🔴 A page that shows or takes a secret must stay no-store. A writer that left this set stopped "+
			"saying it; a new one goes in noStoreWriters with what it serves.", got, want)
	}
	if got, want := strings.Join(literals, ","), strings.Join(sortedKeys(noStoreLiterals), ","); got != want {
		t.Errorf("the functions with a no-store literal are\n  %s\nthe ledger names\n  %s\n"+
			"A response that stopped saying no-store, or a new one: name it in noStoreLiterals.", got, want)
	}
	if strings.Join(views, ",") != strings.Join(viewDocumentCallers, ",") {
		t.Errorf("viewDocument (cacheable: Back may restore it) is called from %v; the ledger names %v.\n"+
			"Only the agent page, the Rendered view and the terminals are restorable. A page with a "+
			"secret on it never is.", views, viewDocumentCallers)
	}
}

// TestTheSecretPagesAreNoStoreAndOnlyTheViewsAreRestorable is the
// behavioural half: each page, through the handler chain, says what the ledger
// says it does.
func TestTheSecretPagesAreNoStoreAndOnlyTheViewsAreRestorable(t *testing.T) {
	f := newGoTrueFixtureWith(t, &Extensions{
		Attention:   emptyAttention{},
		Plugins:     &countingPlugins{},
		Credentials: &memCreds{},
	})
	f.fake.EnablePasskeys()
	f.s.gt.Accounts = &memAccounts{rows: f.store}
	sub := f.member("admin@example.com", authz.OperatorUserID, authz.RoleAdmin)
	at, rt, _ := f.fake.Mint(sub)
	signedIn := []string{"/attention", "/account", "/account/tokens", ui.CredentialsPath, "/account/passkeys",
		"/plugins", "/flows", "/admin/users", "/admin/invites"}
	for _, path := range signedIn {
		w := do(f.edge, "GET", path, nil, withCookies(at, rt), asNavigation)
		if w.Code != http.StatusOK {
			t.Errorf("fixture: GET %s = %d, not the page", path, w.Code)
			continue
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s: Cache-Control = %q, want no-store — it shows or takes a secret", path, got)
		}
	}
	for _, path := range []string{"/login", ClaimPath, InvitePath, ResetPath} {
		w := do(f.edge, "GET", path, nil, asNavigation)
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s (signed out) = %d: Cache-Control = %q, want no-store", path, w.Code, got)
		}
	}

	// The four views: restorable, never shared.
	ms, mts := viewServer(t, goldHost())
	ss, sts := sendServer(t, newSendAgents(), &turnGateway{})
	views := []struct {
		s    *Server
		ts   *httptest.Server
		path string
	}{
		{ms, mts, ref("scratch2", "@7").ViewPath()},
		{ms, mts, ref("scratch2", "@4").TerminalPath()},
		{ss, sts, "/agents/keen-wren?session=41"},
		{ss, sts, ui.AgentTerminalPath("keen-wren")},
	}
	for _, v := range views {
		resp, _ := mirrorGet(t, v.s, v.ts, v.path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("fixture: GET %s = %d", v.path, resp.StatusCode)
			continue
		}
		if got := resp.Header.Get("Cache-Control"); got != viewDocumentCacheControl {
			t.Errorf("GET %s: Cache-Control = %q, want %q", v.path, got, viewDocumentCacheControl)
		}
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// noStoreScan reads dir's non-test Go files and returns, sorted and unique: the
// functions calling noStore, formDocument or writeDocumentHeaders (and those
// helpers themselves); the functions holding a "no-store" string literal; and
// the functions calling viewDocument.
func noStoreScan(t *testing.T, dir string) (writers, literals, views []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	w, l, v := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok {
						switch id.Name {
						case "noStore", "formDocument", "writeDocumentHeaders":
							w[fd.Name.Name] = true
						case "viewDocument":
							v[fd.Name.Name] = true
						}
					}
				case *ast.BasicLit:
					if x.Kind == token.STRING && strings.Contains(x.Value, "no-store") {
						l[fd.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	keys := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	return keys(w), keys(l), keys(v)
}
