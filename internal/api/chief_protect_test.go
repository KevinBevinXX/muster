package api

import (
	"context"
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// DELETION PROTECTION FOR THE CHIEF — every destroy path, and the menu.
//
// The fixture has BOTH kinds of chief on purpose, because they are different
// rows on a real fleet (chief_agent.go's header), and each is the only thing
// that can tell a guard that covers one half from a guard that covers both:
//
//	11  slug "chief"        display "fleet boss"  the RESERVED SLUG, relabelled
//	23  slug "zesty-stoat"  display "Chief"       what chiefAgent RESOLVES
//	31  slug "late-chief"   display "chief"       a second label, HIGHER id: not the chief
//	47  slug "brave-otter"  display "worker"      an ordinary agent
//
// 31 is the control for the tie-break: a guard widened to "any agent labelled
// chief" would refuse it, and an operator who mislabelled a worker could then
// never delete it. Ids are pairwise distinct and none is 1, so no answer can
// come out right by accident of a zero value or a first row.
// ---------------------------------------------------------------------------

type chiefFixtureAgents struct {
	agents.Store
	rows    []agents.Agent
	links   map[int64]int64 // note id -> agent id
	listErr error
}

func (s *chiefFixtureAgents) List(context.Context) ([]agents.Agent, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return append([]agents.Agent(nil), s.rows...), nil
}

func (s *chiefFixtureAgents) Get(_ context.Context, id int64) (agents.Agent, error) {
	for _, a := range s.rows {
		if a.ID == id {
			return a, nil
		}
	}
	return agents.Agent{}, errors.New("no such agent")
}

func (s *chiefFixtureAgents) AgentsByNoteIDs(_ context.Context, ids []int64) (map[int64]agents.Agent, error) {
	out := map[int64]agents.Agent{}
	for _, nid := range ids {
		if aid, ok := s.links[nid]; ok {
			a, _ := s.Get(context.Background(), aid)
			out[nid] = a
		}
	}
	return out, nil
}

func (s *chiefFixtureAgents) LastMessageByAgentIDs(context.Context, []int64) (map[int64]time.Time, error) {
	return map[int64]time.Time{}, nil
}

type chiefFixtureNotes struct {
	notes.Store
	mu      sync.Mutex
	deleted []int64
}

func (s *chiefFixtureNotes) Get(_ context.Context, id int64) (notes.Note, error) {
	return notes.Note{ID: id, Title: "task", Status: notes.StatusOpen}, nil
}

func (s *chiefFixtureNotes) SoftDelete(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, id)
	return nil
}

func (s *chiefFixtureNotes) dismissed() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.deleted...)
}

// destroyRecorder is a provisioner whose Destroy reports every id it is handed.
type destroyRecorder struct {
	stubProvisioner
	ch chan int64
}

func (p *destroyRecorder) Destroy(_ context.Context, id int64) error { p.ch <- id; return nil }

const (
	chiefSlugID     int64 = 11
	chiefResolvedID int64 = 23
	lateLabelID     int64 = 31
	workerID        int64 = 47
)

func chiefFixture() (*Server, http.Handler, *chiefFixtureAgents, *chiefFixtureNotes, *destroyRecorder) {
	as := &chiefFixtureAgents{
		rows: []agents.Agent{
			{ID: workerID, Name: "brave-otter", DisplayName: "worker", Status: agents.StatusRunning},
			{ID: lateLabelID, Name: "late-chief", DisplayName: "chief", Status: agents.StatusStopped},
			{ID: chiefResolvedID, Name: "zesty-stoat", DisplayName: " Chief ", Status: agents.StatusRunning},
			{ID: chiefSlugID, Name: agents.ChiefName, DisplayName: "fleet boss", Status: agents.StatusStopped},
		},
		links: map[int64]int64{101: chiefResolvedID, 103: chiefSlugID, 102: workerID, 104: lateLabelID},
	}
	ns := &chiefFixtureNotes{}
	prov := &destroyRecorder{ch: make(chan int64, 8)}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(os.Stderr, "", 0))
	s.UseExtensions(Extensions{Agents: as, Notes: ns, Provisioner: prov, SessionLiveness: stubLiveness{}})
	return s, s.Handler(), as, ns, prov
}

// destroyedWithin collects every Destroy id handed over within d. The teardown
// runs in a goroutine (safeGo), so absence is read over a window; the window is
// the SAME one in which the ordinary-agent cases below observe their Destroy,
// so a "nothing destroyed" here is measured by an instrument shown to see one.
func destroyedWithin(p *destroyRecorder, d time.Duration) []int64 {
	var got []int64
	deadline := time.After(d)
	for {
		select {
		case id := <-p.ch:
			got = append(got, id)
		case <-deadline:
			return got
		}
	}
}

const destroyWindow = 400 * time.Millisecond

func doReq(t *testing.T, s *Server, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	admit(s, req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestTheChiefCannotBeDeletedFromItsCard: DELETE /agents/{id} — the ⋯ menu's
// Confirm delete — answers 409 for BOTH chiefs and tears nothing down, while an
// ordinary agent and a mislabelled one are still deleted.
func TestTheChiefCannotBeDeletedFromItsCard(t *testing.T) {
	for _, tc := range []struct {
		name    string
		id      int64
		protect bool
	}{
		{"reserved slug", chiefSlugID, true},
		{"resolved chief", chiefResolvedID, true},
		{"second chief label (not the chief)", lateLabelID, false},
		{"ordinary agent", workerID, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, h, _, _, prov := chiefFixture()
			rec := doReq(t, s, h, http.MethodDelete, "/agents/"+itoa(tc.id))
			got := destroyedWithin(prov, destroyWindow)
			if tc.protect {
				if rec.Code != http.StatusConflict {
					t.Fatalf("DELETE /agents/%d = %d, want 409 (it is the chief). body: %s", tc.id, rec.Code, rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), "chief agent cannot be deleted") {
					t.Errorf("the 409 does not say why: %q", rec.Body.String())
				}
				// Only a display-name chief has a way out, so only its refusal may
				// offer one: the reserved slug is protected absolutely.
				if got, want := strings.Contains(rec.Body.String(), "rename it first"), tc.id != chiefSlugID; got != want {
					t.Errorf("the 409 advises renaming = %v, want %v: %q", got, want, rec.Body.String())
				}
				if len(got) != 0 {
					t.Errorf("the chief was refused AND torn down anyway: Destroy(%v)", got)
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("DELETE /agents/%d = %d, want 200. body: %s", tc.id, rec.Code, rec.Body.String())
			}
			if len(got) != 1 || got[0] != tc.id {
				t.Errorf("Destroy calls = %v, want exactly [%d]", got, tc.id)
			}
		})
	}
}

// TestAChiefCheckThatCannotReadFailsClosed: when the agent table cannot be read
// the delete is refused and nothing is torn down — a refused delete can be
// retried, a destroyed chief cannot be undone.
func TestAChiefCheckThatCannotReadFailsClosed(t *testing.T) {
	s, h, as, _, prov := chiefFixture()
	as.listErr = errors.New("database is down")
	rec := doReq(t, s, h, http.MethodDelete, "/agents/"+itoa(workerID))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE with an unreadable agent table = %d, want 500", rec.Code)
	}
	if got := destroyedWithin(prov, destroyWindow); len(got) != 0 {
		t.Errorf("an unverified delete tore down %v", got)
	}
}

// TestDismissingATaskNeverDestroysTheChief: both task-delete routes — the
// board's DELETE /tasks/{id} and the machine DELETE /api/tasks/{id} — dismiss
// the task in every case, tear down an ordinary linked agent as before, and
// leave a linked chief running.
func TestDismissingATaskNeverDestroysTheChief(t *testing.T) {
	for _, route := range []string{"/tasks/", "/api/tasks/"} {
		for _, tc := range []struct {
			note        int64
			wantDestroy []int64
		}{
			{101, nil},                  // linked to the resolved chief
			{103, nil},                  // linked to the reserved slug
			{102, []int64{workerID}},    // ordinary agent: torn down as before
			{104, []int64{lateLabelID}}, // a second "chief" label is not the chief
		} {
			t.Run(route+itoa(tc.note), func(t *testing.T) {
				s, h, _, ns, prov := chiefFixture()
				rec := doReq(t, s, h, http.MethodDelete, route+itoa(tc.note))
				if rec.Code != http.StatusOK {
					t.Fatalf("DELETE %s%d = %d, want 200 (the TASK is always dismissed). body: %s", route, tc.note, rec.Code, rec.Body.String())
				}
				if d := ns.dismissed(); len(d) != 1 || d[0] != tc.note {
					t.Errorf("dismissed tasks = %v, want [%d]", d, tc.note)
				}
				got := destroyedWithin(prov, destroyWindow)
				if !equalIDs(got, tc.wantDestroy) {
					t.Errorf("Destroy calls = %v, want %v", got, tc.wantDestroy)
				}
			})
		}
	}
}

// TestADismissWhoseChiefCheckCannotReadLeavesTheAgent: an unreadable agent
// table still dismisses the task, and tears down NOTHING — the linked agent
// here is an ordinary one, so only the fail-closed branch can keep it alive.
func TestADismissWhoseChiefCheckCannotReadLeavesTheAgent(t *testing.T) {
	s, h, as, ns, prov := chiefFixture()
	as.listErr = errors.New("database is down")
	rec := doReq(t, s, h, http.MethodDelete, "/tasks/102")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /tasks/102 = %d, want 200: the dismiss does not depend on the chief check", rec.Code)
	}
	if d := ns.dismissed(); len(d) != 1 || d[0] != 102 {
		t.Errorf("dismissed tasks = %v, want [102]", d)
	}
	if got := destroyedWithin(prov, destroyWindow); len(got) != 0 {
		t.Errorf("an unverified dismiss tore down %v", got)
	}
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestTheChiefsCardOffersNoDelete: the card list reads the same predicate, so
// the menu never offers a Delete the server refuses — and still offers it on
// every other card (a menu that dropped Delete everywhere would pass a check
// that only looked at the chief).
func TestTheChiefsCardOffersNoDelete(t *testing.T) {
	s, h, _, _, _ := chiefFixture()
	body := getOwed(t, s, h, "/ui/agents").Body.String()
	for _, tc := range []struct {
		id         int64
		wantDelete bool
	}{
		{chiefSlugID, false}, {chiefResolvedID, false}, {lateLabelID, true}, {workerID, true},
	} {
		card := cardChunk(t, body, tc.id)
		has := strings.Contains(card, `hx-delete="/agents/`+itoa(tc.id)+`"`)
		if has != tc.wantDelete {
			t.Errorf("agent %d's card offers delete = %v, want %v", tc.id, has, tc.wantDelete)
		}
		// The menu itself is still there: Start/Stop stays available to the chief.
		if !strings.Contains(card, `aria-label="Agent actions"`) {
			t.Errorf("agent %d's card has no ⋯ menu at all", tc.id)
		}
	}
}

// cardChunk is one agent's card: from its <article> to the next one.
func cardChunk(t *testing.T, markup string, id int64) string {
	t.Helper()
	start := strings.Index(markup, `<article id="agent-`+itoa(id)+`"`)
	if start < 0 {
		t.Fatalf("no card for agent %d", id)
	}
	rest := markup[start+1:]
	if end := strings.Index(rest, `<article id="agent-`); end >= 0 {
		return markup[start : start+1+end]
	}
	return markup[start:]
}

// TestEveryDestroyCallSiteIsChiefGuarded is the LEDGER of every top-level
// declaration in this package that can reach the provisioner's Destroy.
//
// 🔴 IT IS RESOLVED BY TYPE, NOT BY SPELLING, AND IT WALKS WHOLE FILES. Earlier
// versions each had a shape they could not see, found by audit rather than by
// the test: matching the literal selector `X.Provisioner.Destroy` missed
// `p := s.ext.Provisioner; p.Destroy(id)`; walking only function BODIES missed a
// package-level `var x = func(s *Server) … { return s.ext.Provisioner.Destroy }`;
// and matching only (Provisioner).Destroy missed a narrower local interface
// (`type d interface{ Destroy(context.Context, int64) error }`) assigned from the provisioner.
//
// ⚠ WHAT IT PROVES, EXACTLY, AND NO MORE. A "use" is any selector, anywhere in a
// non-test file of THIS package, that type-checks to a method named Destroy
// whose receiver is api.Provisioner OR any interface api.Provisioner satisfies —
// called or merely taken as a value. Each use is attributed to its enclosing
// top-level declaration (a func, or a package-level var/const/type). It proves:
//
//	(1) the SET of such declarations is exactly the two named below — it fails
//	    when one is added, including outside any function, or removed; and
//	(2) each of them also mentions agentIsChief.
//
// It does NOT prove:
//   - that agentIsChief's ANSWER gates the destroy (a function can call it and
//     ignore it) — the behavioural tests above prove that for each named route;
//   - anything about a destroy reached by REFLECTION, through an `any`, or
//     through a concrete provisioner type rather than an interface; or
//   - anything outside internal/api.
//
// 🔴 THE LEDGER CARRIES ITS OWN POSITIVE CONTROLS, IN THE SAME TYPE-CHECK: one
// control declaration per shape above is added to the checked file set and must
// be FOUND, or the two real names were matched by an instrument not shown to
// see the spellings it claims to. (One check: the source importer costs ~12s.)
func TestEveryDestroyCallSiteIsChiefGuarded(t *testing.T) {
	controls := map[string]bool{
		"ledgerControlAlias":  true, // through a local variable
		"ledgerControlValue":  true, // a bare method value
		"ledgerControlPkgLit": true, // a package-level func literal, outside any FuncDecl
		"ledgerControlNarrow": true, // a narrower local interface
	}
	users := destroyUsers(t, map[string]string{"zz_ledger_control.go": `package api

import "context"

func (s *Server) ledgerControlAlias() { p := s.ext.Provisioner; _ = p.Destroy(context.Background(), 1) }
func (s *Server) ledgerControlValue() { f := s.ext.Provisioner.Destroy; _ = f }

var ledgerControlPkgLit = func(s *Server) func(context.Context, int64) error { return s.ext.Provisioner.Destroy }

func (s *Server) ledgerControlNarrow() {
	type destroyer interface{ Destroy(context.Context, int64) error }
	var d destroyer = s.ext.Provisioner
	_ = d.Destroy(context.Background(), 1)
}
`})
	var names []string
	for n := range users {
		if controls[n] {
			delete(controls, n)
			continue
		}
		names = append(names, n)
	}
	if len(controls) != 0 {
		t.Fatalf("the ledger did not see its own controls %v — the list below proves nothing "+
			"about that shape", controls)
	}
	sort.Strings(names)
	if got, want := strings.Join(names, ","), "dismissTask,handleAgentDelete,replaceChief"; got != want {
		t.Fatalf("declarations that can reach the provisioner's Destroy = %s, want %s — a destroy "+
			"path was added or removed; guard it with agentIsChief and add its route to the tests above", got, want)
	}
	// 🔴 replaceChief IS THE ONE DELIBERATE BYPASS: it exists to destroy the
	// chief, behind an explicit confirm (chief_replace.go;
	// TestReplacingTheChiefNeedsConfirmAndDeletesOnlyThen pins the confirm). Every
	// other destroy path must still ask agentIsChief.
	bypass := map[string]bool{"replaceChief": true}
	for _, n := range names {
		if !bypass[n] && !users[n] {
			t.Errorf("%s can reach the provisioner's Destroy without mentioning agentIsChief", n)
		}
	}
}

// destroyUsers type-checks this package's non-test files (plus extra, name ->
// source) and returns each top-level declaration that uses a provisioner
// Destroy (see the ledger's doc for what counts), mapped to whether it also
// mentions agentIsChief.
func destroyUsers(t *testing.T, extra map[string]string) map[string]bool {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, f := range paths {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	for name, src := range extra {
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	pkg, err := conf.Check("github.com/ZacxDev/muster/internal/api", fset, files, info)
	if err != nil {
		t.Fatalf("type-check: %v", err)
	}
	prov := pkg.Scope().Lookup("Provisioner").Type()
	if _, ok := prov.Underlying().(*types.Interface); !ok {
		t.Fatal("api.Provisioner is not an interface any more; this ledger needs rewriting")
	}
	// isDestroy: a method named Destroy on Provisioner itself or on any
	// interface Provisioner satisfies (the narrower-interface shape).
	isDestroy := func(sel *types.Selection) bool {
		if sel == nil || sel.Obj().Name() != "Destroy" {
			return false
		}
		if _, ok := sel.Obj().(*types.Func); !ok {
			return false
		}
		recv := sel.Recv()
		if types.Identical(recv, prov) {
			return true
		}
		iface, ok := recv.Underlying().(*types.Interface)
		return ok && types.Implements(prov, iface)
	}
	out := map[string]bool{}
	for _, file := range files {
		for _, d := range file.Decls {
			name := declName(d)
			var uses, guarded bool
			ast.Inspect(d, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if isDestroy(info.Selections[sel]) {
					uses = true
				}
				if sel.Sel.Name == "agentIsChief" {
					guarded = true
				}
				return true
			})
			if uses {
				out[name] = guarded
			}
		}
	}
	return out
}

// declName names a top-level declaration: a func by its name, anything else by
// the first name it declares.
func declName(d ast.Decl) string {
	switch d := d.(type) {
	case *ast.FuncDecl:
		return d.Name.Name
	case *ast.GenDecl:
		for _, sp := range d.Specs {
			switch sp := sp.(type) {
			case *ast.ValueSpec:
				if len(sp.Names) > 0 {
					return sp.Names[0].Name
				}
			case *ast.TypeSpec:
				return sp.Name.Name
			}
		}
	}
	return "(unnamed declaration)"
}
