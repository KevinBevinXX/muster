package mirror_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestEveryServerPasteGoesThroughPasteWindow is a SEAM LEDGER over this
// package's source, not a behaviour test: the behaviour is
// TestAPasteIntoAHermesWindowIsRefusedUnlessItsHostAdvertisedHermesSend, and
// this is what keeps it complete. The hermesSend switch sits in
// Hub.PasteWindow, so:
//
//  1. a paste message (a Msg whose Type is TypePaste) is BUILT in exactly one
//     function, Hub.pasteTo;
//  2. pasteTo is CALLED from exactly one, Hub.PasteWindow — and Hub.Paste,
//     the other entry point, only delegates to PasteWindow;
//  3. inside PasteWindow the switch (hermesSendGate) runs BEFORE pasteTo;
//  4. outside internal/mirror and the host (internal/mirrorhost, which
//     RECEIVES pastes), no source file names TypePaste: no other package can
//     write one onto a host's connection.
//
// A new way to paste fails here until it is routed through PasteWindow.
func TestEveryServerPasteGoesThroughPasteWindow(t *testing.T) {
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var builds, calls, delegates []string
	gateBeforePaste := false
	files := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files++
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			where := fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				switch r := fd.Recv.List[0].Type.(type) {
				case *ast.Ident:
					where = r.Name + "." + where
				case *ast.StarExpr:
					if id, ok := r.X.(*ast.Ident); ok {
						where = id.Name + "." + where
					}
				}
			}
			var gate, paste token.Pos
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					for _, el := range x.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Type" {
							if v, ok := kv.Value.(*ast.Ident); ok && v.Name == "TypePaste" {
								builds = append(builds, where)
							}
						}
					}
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					switch sel.Sel.Name {
					case "pasteTo":
						calls = append(calls, where)
						paste = x.Pos()
					case "PasteWindow":
						delegates = append(delegates, where)
					case "hermesSendGate":
						gate = x.Pos()
					}
				}
				return true
			})
			if where == "Hub.PasteWindow" && gate.IsValid() && paste.IsValid() && gate < paste {
				gateBeforePaste = true
			}
		}
	}
	if files < 10 {
		t.Fatalf("instrument check: parsed %d source files; is the test running in the package directory?", files)
	}
	if want := []string{"Hub.pasteTo"}; !slices.Equal(builds, want) {
		t.Errorf("a paste message is built in %q; want exactly %q", builds, want)
	}
	if want := []string{"Hub.PasteWindow"}; !slices.Equal(calls, want) {
		t.Errorf("pasteTo is called from %q; want exactly %q — every paste goes through PasteWindow and its hermesSend switch", calls, want)
	}
	slices.Sort(delegates)
	if want := []string{"Hub.Paste", "Service.Paste", "Service.PasteWindow"}; !slices.Equal(delegates, want) {
		t.Errorf("PasteWindow is called from %q; want exactly %q (the entry points that delegate to it)", delegates, want)
	}
	if !gateBeforePaste {
		t.Error("Hub.PasteWindow does not run hermesSendGate before pasteTo")
	}

	// 4: no other package names TypePaste.
	root := filepath.Join("..", "..")
	var named []string
	scanned := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			if strings.HasPrefix(rel, filepath.Join("internal", "mirror")+string(filepath.Separator)) ||
				strings.HasPrefix(rel, filepath.Join("internal", "mirrorhost")+string(filepath.Separator)) {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			scanned++
			if strings.Contains(string(b), "TypePaste") {
				named = append(named, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 100 {
		t.Fatalf("instrument check: scanned %d source files outside internal/mirror; is the walk rooted at the module?", scanned)
	}
	if len(named) > 0 {
		t.Errorf("TypePaste is named outside internal/mirror and the host: %q", named)
	}
}

// TestTheHermesSwitchRefusalSaysMusterRefused: the switch's refusal carries
// the paste guard's words and kind (so a person sees one refusal whichever
// side made it), and is ErrHermesSendOff — muster's own, not the host's.
func TestTheHermesSwitchRefusalSaysMusterRefused(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(asUser(1), 3*time.Second)
	defer cancel()
	tok, _ := r.store.mint(t, 1, "off")
	h := connectJSON(t, r, tok, hermesHello("off", "idle", ""))
	r.online(t, 1, "off", "work")
	err := r.svc.PasteWindow(ctx, "off", "work", "@1", "x", mirror.PasteActivePane)
	if !errors.Is(err, mirror.ErrHermesSendOff) || !errors.Is(err, mirror.ErrPasteRefused) || errors.Is(err, mirror.ErrPasteUnconfirmed) {
		t.Fatalf("the switch's refusal = %v; want ErrHermesSendOff and ErrPasteRefused, never unconfirmed", err)
	}
	if len(h.pasted()) != 0 {
		t.Fatalf("the host received %q", h.pasted())
	}
	// A read-only host's Hermes window: the -interactive refusal comes first,
	// and a hello's hermesSend without -interactive opens nothing.
	tok2, _ := r.store.mint(t, 1, "ro")
	connectJSON(t, r, tok2, strings.Replace(hermesHello("ro", "idle", `,"hermesSend":true`), `"interactive":true`, `"interactive":false`, 1))
	r.online(t, 1, "ro", "work")
	if err := r.svc.PasteWindow(ctx, "ro", "work", "@1", "x", mirror.PasteActivePane); !errors.Is(err, mirror.ErrInteractiveRefused) {
		t.Fatalf("a read-only host's Hermes window = %v; want ErrInteractiveRefused", err)
	}
}

// TestAgentStateNamesTheRuntime: AgentState says which runtime it read —
// also when the status is unknown, because Send now's rule differs by runtime
// — and a Hermes window with no status is unknown with Hermes's own reason.
func TestAgentStateNamesTheRuntime(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "ws")
	connectJSON(t, r, tok, `{"type":"hello","version":2,"host":"ws","interactive":true,"statusReport":true,"windows":[`+
		`{"session":"work","id":"@1","index":0,"active":true,"runtime":"hermes"},`+
		`{"session":"work","id":"@2","index":1,"claude":true},`+
		`{"session":"work","id":"@3","index":2,"runtime":"hermes","status":"idle","statusSince":1792000001111}]}`)
	r.online(t, 1, "ws", "work")
	st := r.svc.AgentState(asUser(1), "ws", "work", "@1")
	if st.Runtime != mirror.RuntimeHermes || !errors.Is(st.Unknown, mirror.ErrNoHermesStatus) {
		t.Fatalf("a Hermes window with no status = %+v; want Runtime hermes, ErrNoHermesStatus", st)
	}
	if st := r.svc.AgentState(asUser(1), "ws", "work", "@2"); st.Runtime != mirror.RuntimeClaude || !errors.Is(st.Unknown, mirror.ErrNoStatus) {
		t.Fatalf("a Claude window with no status = %+v; want Runtime claude, ErrNoStatus", st)
	}
	if st := r.svc.AgentState(asUser(1), "ws", "work", "@3"); st.Runtime != mirror.RuntimeHermes || st.Unknown != nil || st.Status != mirror.StatusIdle {
		t.Fatalf("an idle Hermes window = %+v", st)
	}
}

// TestNormalizeWindowsKeepsRuntimesApart: on the wire a window is Claude Code,
// Hermes, or neither — never both (Hermes, the more restrictive, wins) and
// never a runtime muster does not mirror. A Hermes id of 70 characters is
// kept, one over the 128 bound is not, and a non-Hermes window has none; a
// Claude window's status and id are untouched.
func TestNormalizeWindowsKeepsRuntimesApart(t *testing.T) {
	hermesID := "local-" + strings.Repeat("0123456789abcdef", 4)
	got := mirror.NormalizeWindows([]mirror.Window{
		{Session: "s", ID: "@1", Runtime: mirror.RuntimeHermes, AgentSession: hermesID, Status: mirror.StatusIdle, StatusSince: 5},
		{Session: "s", ID: "@2", Runtime: mirror.RuntimeHermes, Claude: true, ClaudeSession: "abc", Pending: &mirror.Pending{Summary: "x"}, Status: mirror.StatusWaiting},
		{Session: "s", ID: "@3", Runtime: "opencode", AgentSession: "abc", Status: mirror.StatusIdle},
		{Session: "s", ID: "@4", Runtime: mirror.RuntimeHermes, AgentSession: strings.Repeat("a", 129)},
		{Session: "s", ID: "@5", Claude: true, ClaudeSession: "abc", AgentSession: "def", Status: mirror.StatusBusy, StatusSince: 7},
		{Session: "s", ID: "@6", Runtime: mirror.RuntimeClaude, Status: mirror.StatusBusy},
	}, 0)
	if len(got) != 6 {
		t.Fatalf("%+v", got)
	}
	if w := got[0]; w.AgentRuntime() != mirror.RuntimeHermes || w.AgentSession != hermesID || w.Status != mirror.StatusIdle || w.StatusSince != 5 ||
		w.SessionKey() != "hermes:"+hermesID {
		t.Errorf("a Hermes window: %+v (key %q)", w, w.SessionKey())
	}
	if w := got[1]; w.Claude || w.ClaudeSession != "" || w.Pending != nil || w.AgentRuntime() != mirror.RuntimeHermes || w.Status != mirror.StatusWaiting {
		t.Errorf("a window claiming both: %+v; want Hermes alone", w)
	}
	if w := got[2]; w.Runtime != "" || w.AgentSession != "" || w.Status != "" || w.AgentRuntime() != "" {
		t.Errorf("an unknown runtime: %+v; want a plain window", w)
	}
	if w := got[3]; w.AgentSession != "" || w.AgentRuntime() != mirror.RuntimeHermes || w.SessionKey() != "" {
		t.Errorf("an over-long Hermes id: %+v; want the window, without the id", w)
	}
	if w := got[4]; !w.Claude || w.ClaudeSession != "abc" || w.AgentSession != "" || w.Status != mirror.StatusBusy || w.SessionKey() != "abc" {
		t.Errorf("a Claude window: %+v", w)
	}
	if w := got[5]; w.Runtime != "" || w.AgentRuntime() != "" || w.Status != "" {
		t.Errorf(`Runtime "claude" without Claude: %+v; want a plain window (Claude Code is Window.Claude)`, w)
	}
	for key, ok := range map[string]bool{
		"0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b": true, "hermes:" + hermesID: true, hermesID: false,
		"hermes:": false, "claude:abc": false, strings.Repeat("c", 65): false,
	} {
		if mirror.ValidSessionKey(key) != ok {
			t.Errorf("ValidSessionKey(%q) = %t, want %t", key, !ok, ok)
		}
	}
}
