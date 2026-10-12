package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/sse"
)

// 🔴 THE BEHAVIOURAL HALF OF THE SEAM, AND THE ONLY THING THAT PINS THE CLIENT
// AND THE SERVER TO EACH OTHER.
//
// The header ledger next door proves this CLI SENDS the provenance headers. The
// server's own tests prove muster RECORDS them. Neither proves the two AGREE: a
// header-name typo, a case difference, a value the server trims to nothing —
// each leaves both suites green and the feature dead. "Verified in isolation" is
// the vacuous green, and the defect lives in the seam nobody owns.
//
// So this test builds NOTHING itself. It runs the real command tree against the
// real api.Handler backed by real Postgres, and watches one task's thread move
// 0 -> 1.
//
// The 0 is not decoration. A test that only asserts "1 afterwards" cannot tell a
// working seam from a fixture that was already linked, and a zero measured by an
// instrument nobody has seen produce a non-zero is indistinguishable from an
// instrument wired to nothing — so the pair is asserted, and reported as a pair.
//
// DB-gated like every other Postgres-backed test here: without a database it
// SKIPS, and a skipped test is not a passing one. `make test` exports
// MUSTER_TEST_REQUIRE_DB=1, which turns that skip into a FAILURE — so the
// project's own gate cannot produce a green that never exercised this.

const seamHookToken = "seam-hook-token"

// seamLiveness is a SessionLivenessProbe that reports every session as having no
// transcript.
//
// ⚠ IT IS NOT A SHORTCUT. The transcript records belong to another service;
// api.UseExtensions treats a Notes store with NO liveness probe as a readiness
// DEFECT precisely because the alternative is every link rendering "no
// transcript recorded" over live transcripts. Supplying an honest always-false
// probe here keeps the wiring in its supported shape while asserting nothing
// about transcripts, which this test does not measure.
type seamLiveness struct{}

func (seamLiveness) SessionsExisting(context.Context, []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// seamServer stands up the real muster HTTP surface over a real database.
func seamServer(t *testing.T) (*httptest.Server, notes.Store) {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ns := notes.NewPG(pool)
	srv := api.New(sse.New(8), api.AuthConfig{HookToken: seamHookToken}, log.New(io.Discard, "", 0))
	srv.UseExtensions(api.Extensions{Notes: ns, SessionLiveness: seamLiveness{}})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, ns
}

// runSeamCLI drives the real command tree at the real server with a controlled
// environment.
func runSeamCLI(t *testing.T, ts *httptest.Server, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut strings.Builder
	full := append([]string{
		"--api-url", ts.URL,
		"--token", seamHookToken,
		"--env-file", filepath.Join(t.TempDir(), "absent.env"),
	}, args...)
	code := run(context.Background(), testProgName, full, strings.NewReader(""), &out, &errOut,
		func(k string) string { return env[k] }, t.TempDir())
	return code, out.String(), errOut.String()
}

func TestSeamClientToServerMovesTheThreadCount(t *testing.T) {
	ts, ns := seamServer(t)
	ctx := dbtest.AsOperator(context.Background())
	const sessionID = "seam-session-7f3a"

	task, err := ns.Create(ctx, notes.Note{Title: "seam task", Body: "prove the header crosses the wire"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _ = ns.Delete(dbtest.AsOperator(context.Background()), task.ID) })
	id := strconv.FormatInt(task.ID, 10)

	// --- NEGATIVE CONTROL: the same command with NO session in the environment
	// must move nothing. Without this, the 0 -> 1 below could be produced by any
	// request at all rather than by the header specifically.
	if code, _, stderr := runSeamCLI(t, ts, nil, "task", "get", id); code != 0 {
		t.Fatalf("task get (no session) exit %d: %s", code, stderr)
	}
	if links, err := ns.SessionsForTask(ctx, task.ID); err != nil || len(links) != 0 {
		t.Fatalf("a session-less CLI call created %d links (err=%v), want 0", len(links), err)
	}

	// --- THE PAIR. 0 before…
	before, err := ns.SessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("sessions before: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("thread starts at %d, want 0", len(before))
	}

	// …one real `task comment` with the session variable set…
	code, _, stderr := runSeamCLI(t, ts, map[string]string{liveSessionEnvName: sessionID},
		"task", "comment", id, "--body", "picked this up")
	if code != 0 {
		t.Fatalf("task comment exit %d: %s", code, stderr)
	}

	// …and 1 after, read back from the STORE (not from the response the CLI
	// printed), so the assertion cannot be satisfied by the client echoing itself.
	after, err := ns.SessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("sessions after: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("🔴 the thread went 0 -> %d, want 0 -> 1. The CLI sent a request the server did "+
			"not record as a session touch — the two halves of the seam disagree, and both suites "+
			"are green.", len(after))
	}
	if after[0].SessionID != sessionID {
		t.Fatalf("linked session = %q, want %q (the header value did not survive the wire)", after[0].SessionID, sessionID)
	}
	if after[0].Role != notes.RoleWorked {
		t.Fatalf("`task comment` recorded role %q, want %q — commenting is work", after[0].Role, notes.RoleWorked)
	}
	if after[0].Host == "" {
		t.Fatalf("the host header did not reach the stored link: %+v", after[0])
	}

	// A `task get` from the SAME session must not add a second row, and must not
	// downgrade the role — the whole monotonic contract, exercised over the wire.
	if code, _, stderr := runSeamCLI(t, ts, map[string]string{liveSessionEnvName: sessionID}, "task", "get", id); code != 0 {
		t.Fatalf("task get exit %d: %s", code, stderr)
	}
	again, err := ns.SessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("sessions after get: %v", err)
	}
	if len(again) != 1 || again[0].Role != notes.RoleWorked {
		t.Fatalf("a later `task get` produced %+v; want one link still at role worked", again)
	}

	// And the CLI's own output carries the thread, so a caller reading stdout
	// sees what the store holds.
	code, stdout, stderr := runSeamCLI(t, ts, nil, "task", "get", id)
	if code != 0 {
		t.Fatalf("task get exit %d: %s", code, stderr)
	}
	var got struct {
		Sessions []notes.SessionLink `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode CLI stdout: %v", err)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].SessionID != sessionID {
		t.Fatalf("the CLI's own JSON does not carry the thread: %+v", got.Sessions)
	}

	reverse, err := ns.TasksForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if len(reverse) != 1 || reverse[0].NoteID != task.ID {
		t.Fatalf("reverse lookup = %+v, want task %d", reverse, task.ID)
	}
}

// postRawTaskComment posts a task comment over raw HTTP with EXACTLY the headers
// given — deliberately NOT through the command tree.
//
// 🔴 BYPASSING THE CLI IS THE POINT, NOT A SHORTCUT. The request being modelled
// comes from an upstream machine binary built BEFORE the extraction, which exists
// in no tree here and cannot be driven by this repo's command builder — taskHeaders
// only knows the X-Muster-* spellings. A test that went through the CLI would be a
// test of the CLI, and the whole failure was that the CLI's spelling and the
// server's had diverged. So the wire shape is constructed literally.
func postRawTaskComment(t *testing.T, ts *httptest.Server, taskID int64, headers map[string]string, body string) (int, string) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost,
		ts.URL+"/api/tasks/"+strconv.FormatInt(taskID, 10)+"/comments",
		strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+seamHookToken)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post comment: %v", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(out)
}

// TestSeamAStaleLegacyClientIsStillAttributedAndStillLinks is the END-TO-END
// half of issue #25, and the only in-repo measurement of the amended closing
// condition: a comment from an UN-REBUILT client — one that sends only the
// pre-extraction X-Clawgate-* spellings — must be attributed to its producer AND
// must write a real task_sessions row.
//
// 🔴 WHY THE UNIT TESTS ARE NOT ENOUGH HERE. internal/api's tests pin taskSource
// and taskSessionHost: they prove the server now READS both spellings. They say
// nothing about whether the resolved session id reaches the STORE — linkTaskSession
// is a deliberate no-op-with-no-error on an empty id, so the entire write path can
// stay silent while a header-level test is green. That is precisely the shape the
// outage had: `GET /api/tasks/{id}` kept answering a `sessions` array, so the
// feature read as present and simply reported empty. Only a row count moving
// 0 -> 1 against real Postgres distinguishes the two.
//
// The NEGATIVE CONTROL runs first and is not optional: the same request with NO
// provenance headers must be authored `api` and must move the count by nothing.
// Without it, a 1 afterwards could be produced by any request at all rather than by
// the legacy headers specifically — and a zero measured by an instrument nobody has
// watched produce a non-zero is indistinguishable from an instrument wired to
// nothing. Both halves of the pair are asserted and both are reported.
func TestSeamAStaleLegacyClientIsStillAttributedAndStillLinks(t *testing.T) {
	ts, ns := seamServer(t)
	ctx := dbtest.AsOperator(context.Background())
	// Distinct from every other fixture in this file, and distinct from "api" —
	// the value the fall-through emits — so a collapse cannot be mistaken for a hit.
	const (
		staleSessionID = "stale-legacy-session-91c4"
		staleHost      = "stale-legacy-host-91c4"
	)

	task, err := ns.Create(ctx, notes.Note{Title: "stale-client seam task", Body: "issue #25"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _ = ns.Delete(dbtest.AsOperator(context.Background()), task.ID) })

	// --- NEGATIVE CONTROL: no provenance headers at all.
	code, respBody := postRawTaskComment(t, ts, task.ID, nil, "no provenance at all")
	if code != http.StatusOK {
		t.Fatalf("control comment HTTP %d: %s", code, respBody)
	}
	var control struct{ Author string }
	if err := json.Unmarshal([]byte(respBody), &control); err != nil {
		t.Fatalf("decode control response: %v (%s)", err, respBody)
	}
	if control.Author != "api" {
		t.Fatalf("a header-less comment was authored %q, want \"api\" — the control is not measuring "+
			"the default path", control.Author)
	}
	before, err := ns.SessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("sessions before: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("a header-less comment created %d session links, want 0", len(before))
	}

	// --- THE CASE: ONLY the pre-extraction spellings, exactly as a stale upstream
	// client sends them. No X-Muster-* header is present at all.
	code, respBody = postRawTaskComment(t, ts, task.ID, map[string]string{
		"X-Clawgate-Source":     "claude-code",
		"X-Clawgate-Session-Id": staleSessionID,
		"X-Clawgate-Host":       staleHost,
	}, "picked this up from a stale client")
	if code != http.StatusOK {
		t.Fatalf("stale-client comment HTTP %d: %s", code, respBody)
	}
	var stale struct{ Author string }
	if err := json.Unmarshal([]byte(respBody), &stale); err != nil {
		t.Fatalf("decode stale response: %v (%s)", err, respBody)
	}
	// ⚠ Errorf, NOT Fatalf, and deliberately: the author and the session link are
	// TWO independent halves of the same bug, and a Fatalf here would stop the test
	// before the thread-count assertions ran — so the expensive half would never be
	// watched fail at base, which is the only thing that makes it a regression test
	// rather than a guard nobody has seen go red.
	if stale.Author != "claude-code" {
		t.Errorf("🔴 a comment carrying the legacy source spelling with value claude-code was "+
			"authored %q, want \"claude-code\". This is the issue's control row: the ONLY difference "+
			"from a working request is the header NAME, so the author collapsing to %q means the "+
			"rename is still unhandled and every stale binary on every host is still writing "+
			"de-attributed comments.", stale.Author, "api")
	}

	// …and the EXPENSIVE half: a row in the thread, read back from the STORE rather
	// than from the response the server just printed.
	after, err := ns.SessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("sessions after: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("🔴 the thread went 0 -> %d, want 0 -> 1. A stale client's comment did NOT create a "+
			"task_sessions row, so `which sessions worked task N` still answers an empty array — the "+
			"silent zero that disabled both duplicate-work guards: the task-pickup "+
			"`sessions[] | select(.role==\"worked\")` check, and the upstream handoff resolver, which "+
			"exits non-zero reporting \"0 tasks for this session\".", len(after))
	}
	if after[0].SessionID != staleSessionID {
		t.Fatalf("🔴 linked session = %q, want %q — the legacy session-id header did not survive the "+
			"wire into the stored row", after[0].SessionID, staleSessionID)
	}
	if after[0].Role != notes.RoleWorked {
		t.Fatalf("a stale client's `task comment` recorded role %q, want %q — commenting is work",
			after[0].Role, notes.RoleWorked)
	}
	if after[0].Host != staleHost {
		t.Fatalf("🔴 stored host = %q, want %q — the host header is the third renamed one and it "+
			"lands in task_sessions.host, which is a snapshot rather than a read-time join precisely "+
			"so it outlives the transcript", after[0].Host, staleHost)
	}

	// The reverse direction too: the upstream handoff resolver asks "which tasks did
	// THIS session touch", and that is the query that silently answered 0.
	reverse, err := ns.TasksForSession(ctx, staleSessionID)
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if len(reverse) != 1 || reverse[0].NoteID != task.ID {
		t.Fatalf("🔴 tasks-for-session = %+v, want exactly task %d. This is the direction the "+
			"upstream handoff resolver reads; an empty answer here is the "+
			"\"NOTHING RESOLVED — 0 tasks for this session\" refusal that was read as a real zero.",
			reverse, task.ID)
	}
}

// TestSeamTaskGetRecordsAReadNotAWrite is the role half of the seam. Without it,
// "a link exists" could be satisfied by a client that labelled every touch the
// same way, and the whole point of the role is that reading and working are
// different facts about a task.
func TestSeamTaskGetRecordsAReadNotAWrite(t *testing.T) {
	ts, ns := seamServer(t)
	ctx := dbtest.AsOperator(context.Background())
	const sessionID = "seam-read-only-session"

	task, err := ns.Create(ctx, notes.Note{Title: "read-only seam task", Body: "b"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _ = ns.Delete(dbtest.AsOperator(context.Background()), task.ID) })

	code, _, stderr := runSeamCLI(t, ts, map[string]string{liveSessionEnvName: sessionID},
		"task", "get", strconv.FormatInt(task.ID, 10))
	if code != 0 {
		t.Fatalf("task get exit %d: %s", code, stderr)
	}
	links, err := ns.SessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("thread has %d links, want 1", len(links))
	}
	if links[0].Role != notes.RoleRead {
		t.Fatalf("`task get` recorded role %q, want %q", links[0].Role, notes.RoleRead)
	}
}
