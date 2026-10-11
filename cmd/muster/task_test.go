package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestTaskLsFiltersGoOnTheQueryString(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/tasks", http.StatusOK, `[{"id":1}]`)

	got := h.runCLI("task", "ls", "--tag", "auto:dispatch", "--tag", "repo:example",
		"--status", "open", "--status", "in_progress", "--limit", "5", "--summary")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	reqs := h.nonHealthRequests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
	uri := reqs[0].uri
	for _, want := range []string{"tag=auto%3Adispatch", "tag=repo%3Aexample", "status=open", "status=in_progress", "limit=5", "summary=1"} {
		if !strings.Contains(uri, want) {
			t.Fatalf("request URI %q missing %q", uri, want)
		}
	}
}

func TestTaskLsWithoutFiltersSendsNoQuery(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/tasks", http.StatusOK, `[]`)

	if got := h.runCLI("task", "ls"); got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	reqs := h.nonHealthRequests()
	if len(reqs) != 1 || reqs[0].uri != "/api/tasks" {
		t.Fatalf("requests = %+v, want a bare /api/tasks", reqs)
	}
}

func TestTaskCreateBody(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/tasks", http.StatusOK, `{"id":4242}`)

	got := h.runCLI("task", "create",
		"--title", "a title", "--body", "the body", "--directory", "/tmp/x",
		"--repo", "example-repo", "--branch", "main", "--model", "a-model",
		"--tag", "auto:dispatch")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	reqs := h.nonHealthRequests()
	if len(reqs) != 1 || reqs[0].method != http.MethodPost {
		t.Fatalf("requests = %+v", reqs)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(reqs[0].body), &sent); err != nil {
		t.Fatalf("request body not JSON: %v (%q)", err, reqs[0].body)
	}
	want := map[string]any{
		"title": "a title", "body": "the body", "directory": "/tmp/x",
		"repo": "example-repo", "branch": "main", "model": "a-model",
	}
	for k, v := range want {
		if sent[k] != v {
			t.Fatalf("body[%q] = %v, want %v (full body %q)", k, sent[k], v, reqs[0].body)
		}
	}
	if tags, _ := sent["tags"].([]any); len(tags) != 1 || tags[0] != "auto:dispatch" {
		t.Fatalf("tags = %v", sent["tags"])
	}
	// The privilege subsystem is removed: the CLI no longer sends the key.
	if _, ok := sent["privileges"]; ok {
		t.Fatalf("body still carries privileges: %q", reqs[0].body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &out); err != nil || out["id"].(float64) != 4242 {
		t.Fatalf("stdout = %q (err=%v)", got.stdout, err)
	}
}

// TestTaskCreateMinimalBodyOmitsEmptyKeys: an absent key leaves the server's
// existing default alone, which is NOT the same as sending "".
func TestTaskCreateMinimalBodyOmitsEmptyKeys(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/tasks", http.StatusOK, `{"id":1}`)

	if got := h.runCLI("task", "create", "--body", "just a body"); got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(h.nonHealthRequests()[0].body), &sent)
	if len(sent) != 1 {
		t.Fatalf("minimal create sent %v, want only the body key", sent)
	}
	if sent["body"] != "just a body" {
		t.Fatalf("body = %v", sent["body"])
	}
}

func TestTaskCreateBodyFromStdin(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/tasks", http.StatusOK, `{"id":9}`)

	got := h.runCLIStdin(strings.NewReader("piped body\n"), "task", "create", "--body-file", "-")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(h.nonHealthRequests()[0].body), &sent)
	if sent["body"] != "piped body\n" {
		t.Fatalf("body = %q", sent["body"])
	}
}

func TestTaskCreateRejectsEmptyBodyLocally(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/tasks", http.StatusOK, `{"id":1}`)

	got := h.runCLI("task", "create", "--body", "   ")
	if got.code != exitUsage {
		t.Fatalf("exit = %d, want %d", got.code, exitUsage)
	}
	if n := len(h.requests()); n != 0 {
		t.Fatalf("server saw %d requests, want 0", n)
	}
}

func TestTaskCreateRejectsBothBodySources(t *testing.T) {
	h := newHarness(t)
	got := h.runCLI("task", "create", "--body", "x", "--body-file", "/nonexistent")
	if got.code != exitUsage {
		t.Fatalf("exit = %d, want %d (stderr=%q)", got.code, exitUsage, got.stderr)
	}
	if !strings.Contains(got.stderr, "mutually exclusive") {
		t.Fatalf("stderr = %q", got.stderr)
	}
}

// ---------------------------------------------------------------------------
// task status
// ---------------------------------------------------------------------------

// TestCLIVocabularyIsTheSharedDefinition pins that this CLI reads the shared
// vocabulary rather than reintroducing a local copy.
//
// ⚠ IT IS A GUARD AGAINST RE-FORKING, not a drift check — there is one
// definition, so there is nothing to drift. The upstream version of this test
// compared two copies and could only see drift in ONE direction: a status added
// server-side left it green while the CLI refused the new value at exit 2.
func TestCLIVocabularyIsTheSharedDefinition(t *testing.T) {
	want := taskstatusAll()
	if len(taskStatuses) != len(want) {
		t.Fatalf("CLI vocabulary %v differs in size from taskstatus.All() %v — a local copy has been reintroduced", taskStatuses, want)
	}
	for i := range want {
		if taskStatuses[i] != want[i] {
			t.Fatalf("CLI vocabulary[%d] = %q, want %q (full: %v)", i, taskStatuses[i], want[i], taskStatuses)
		}
	}
	if !validTaskStatus(want[0]) {
		t.Fatalf("validTaskStatus rejects %q, which taskstatus.All() advertises", want[0])
	}
}

func TestTaskStatusSendsPatchWithStatusBody(t *testing.T) {
	h := newHarness(t)
	h.json("PATCH /api/tasks/42/status", http.StatusOK, `{"id":42,"status":"in_progress"}`)

	got := h.runCLI("task", "status", "42", "in_progress")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	reqs := h.nonHealthRequests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
	if reqs[0].method != http.MethodPatch || reqs[0].uri != "/api/tasks/42/status" {
		t.Fatalf("got %s %s, want PATCH /api/tasks/42/status", reqs[0].method, reqs[0].uri)
	}
	// Pins the credential on this route. Found unpinned upstream by audit:
	// dropping it left the suite green, and the failure mode (401 -> exit 3) is
	// loud but only at runtime.
	if reqs[0].auth != "Bearer "+testToken {
		t.Fatalf("Authorization = %q, want the hook token", reqs[0].auth)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(reqs[0].body), &sent); err != nil {
		t.Fatalf("body not JSON: %v (%q)", err, reqs[0].body)
	}
	if len(sent) != 1 || sent["status"] != "in_progress" {
		t.Fatalf("body = %v, want exactly {status: in_progress}", sent)
	}
}

// TestTaskStatusAcceptsEveryValidStatus covers `complete` explicitly: the agent
// self-service route forbids it, and this producer route is the one path that
// allows it. A guard that accidentally rejected it would look correct.
func TestTaskStatusAcceptsEveryValidStatus(t *testing.T) {
	for _, status := range taskStatuses {
		t.Run(status, func(t *testing.T) {
			h := newHarness(t)
			h.json("PATCH /api/tasks/7/status", http.StatusOK, `{"id":7}`)

			got := h.runCLI("task", "status", "7", status)
			if got.code != exitOK {
				t.Fatalf("status %q: exit = %d, stderr=%q", status, got.code, got.stderr)
			}
			var sent map[string]any
			_ = json.Unmarshal([]byte(h.nonHealthRequests()[0].body), &sent)
			if sent["status"] != status {
				t.Fatalf("sent %v, want %q", sent["status"], status)
			}
		})
	}
}

// TestTaskStatusRejectsUnknownStatusLocally asserts the guard fires with ITS
// OWN message and sends nothing. Asserting only "exit 2" would still pass if
// some other check (arg count, path expansion) rejected the call first, which
// is the classic unreachable-guard false green.
func TestTaskStatusRejectsUnknownStatusLocally(t *testing.T) {
	for _, bad := range []string{"in-progress", "readyforreview", "dismissed", "todo", ""} {
		t.Run("status="+bad, func(t *testing.T) {
			h := newHarness(t)
			h.json("PATCH /api/tasks/1/status", http.StatusOK, `{"id":1}`)

			got := h.runCLI("task", "status", "1", bad)
			if got.code != exitUsage {
				t.Fatalf("exit = %d, want %d (stderr=%q)", got.code, exitUsage, got.stderr)
			}
			if !strings.Contains(got.stderr, invalidStatusMsg) {
				t.Fatalf("stderr %q does not carry this guard's own message %q", got.stderr, invalidStatusMsg)
			}
			// The message must name the vocabulary — that is the whole reason the
			// check is client-side rather than left to the server's bare "invalid or
			// missing status".
			for _, want := range taskStatuses {
				if !strings.Contains(got.stderr, want) {
					t.Fatalf("stderr %q does not list the valid status %q", got.stderr, want)
				}
			}
			if n := len(h.requests()); n != 0 {
				t.Fatalf("server saw %d requests, want 0 — a rejected status must never reach the wire", n)
			}
		})
	}
}

func TestTaskStatusNotFoundExitsFour(t *testing.T) {
	h := newHarness(t)
	h.json("PATCH /api/tasks/999/status", http.StatusNotFound, `{"error":"task not found"}`)

	got := h.runCLI("task", "status", "999", "open")
	if got.code != exitNotFound {
		t.Fatalf("exit = %d, want %d (stderr=%q)", got.code, exitNotFound, got.stderr)
	}
}

// TestTaskStatusTrimsTheStatusArgument pins the TrimSpace on the status arg —
// the shell-newline case (`task status "$ID" "$(…)"`) it exists for.
func TestTaskStatusTrimsTheStatusArgument(t *testing.T) {
	h := newHarness(t)
	h.json("PATCH /api/tasks/9/status", http.StatusOK, `{"id":9}`)

	got := h.runCLI("task", "status", "9", "  in_progress\n")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(h.nonHealthRequests()[0].body), &sent)
	if sent["status"] != "in_progress" {
		t.Fatalf("sent %q, want the trimmed %q", sent["status"], "in_progress")
	}
}

// ---------------------------------------------------------------------------
// task comment
// ---------------------------------------------------------------------------

func TestTaskCommentPostsBodyAndDefaultSourceHeader(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/tasks/12/comments", http.StatusOK,
		`{"id":5,"noteId":12,"author":"claude-code","body":"done"}`)

	got := h.runCLI("task", "comment", "12", "--body", "done")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	reqs := h.nonHealthRequests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
	if reqs[0].method != http.MethodPost || reqs[0].uri != "/api/tasks/12/comments" {
		t.Fatalf("got %s %s", reqs[0].method, reqs[0].uri)
	}
	if reqs[0].source != defaultTaskSource {
		t.Fatalf("%s = %q, want %q", headerSource, reqs[0].source, defaultTaskSource)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(reqs[0].body), &sent); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	// Exactly one key. An `author` key would be dropped by the server anyway,
	// but sending one would signal an intent the impersonation guard forbids.
	if len(sent) != 1 || sent["body"] != "done" {
		t.Fatalf("body = %v, want exactly {body: done}", sent)
	}
}

func TestTaskCommentCustomSource(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/tasks/3/comments", http.StatusOK, `{"id":1,"author":"repo-cos"}`)

	got := h.runCLI("task", "comment", "3", "--body", "x", "--source", "repo-cos")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	if src := h.nonHealthRequests()[0].source; src != "repo-cos" {
		t.Fatalf("%s = %q", headerSource, src)
	}
}

// TestTaskCommentWarnsOnSilentAttributionDowngrade: an unrecognised source is
// NOT an error server-side — the comment is silently authored as "api". The
// warning turns that into something an operator can see, without failing the
// command (the comment was posted; only its attribution differs).
func TestTaskCommentWarnsOnSilentAttributionDowngrade(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/tasks/8/comments", http.StatusOK, `{"id":2,"author":"api","body":"x"}`)

	got := h.runCLI("task", "comment", "8", "--body", "x", "--source", "not-on-the-allowlist")
	if got.code != exitOK {
		t.Fatalf("exit = %d, want success — the comment WAS posted (stderr=%q)", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "silently downgraded") {
		t.Fatalf("stderr = %q, want an attribution warning", got.stderr)
	}
	if !strings.Contains(got.stderr, "not-on-the-allowlist") || !strings.Contains(got.stderr, `"api"`) {
		t.Fatalf("stderr = %q, want it to name BOTH the requested and the recorded author", got.stderr)
	}
	// stdout stays pure JSON — the note must not corrupt a `| jq` pipeline.
	var out map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &out); err != nil {
		t.Fatalf("stdout is not valid JSON: %v (%q)", err, got.stdout)
	}
}

// TestTaskCommentSilentWhenAttributionMatches is the negative control for the
// warning above: without it, a warning that fired unconditionally would pass
// the previous test while being useless.
func TestTaskCommentSilentWhenAttributionMatches(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/tasks/8/comments", http.StatusOK, `{"id":2,"author":"claude-code"}`)

	got := h.runCLI("task", "comment", "8", "--body", "x")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	if strings.Contains(got.stderr, "silently downgraded") {
		t.Fatalf("warned about attribution when the author matched: stderr=%q", got.stderr)
	}
}

// TestTaskCommentTrimsSourceBeforeComparing is a REGRESSION test for a live
// defect carried over from upstream: the server trims the source header, so
// comparing the untrimmed request value against the trimmed author it returns
// accused a perfectly correct call of having been silently downgraded.
func TestTaskCommentTrimsSourceBeforeComparing(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/tasks/8/comments", http.StatusOK, `{"id":2,"author":"claude-code","body":"x"}`)

	got := h.runCLI("task", "comment", "8", "--body", "x", "--source", "  claude-code  ")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	if strings.Contains(got.stderr, "silently downgraded") {
		t.Fatalf("padded --source produced a FALSE downgrade warning: %q", got.stderr)
	}
	// The header must carry the trimmed value too, not merely compare as one.
	if src := h.nonHealthRequests()[0].source; src != "claude-code" {
		t.Fatalf("%s = %q, want the trimmed %q", headerSource, src, "claude-code")
	}
}

// TestTaskCommentHasNoAuthorFlag is a structural guard on the impersonation
// property: the author is a header the server controls, so offering an --author
// flag would promise something the API deliberately cannot honour.
func TestTaskCommentHasNoAuthorFlag(t *testing.T) {
	root := newRootCmd(inspectApp())
	for _, c := range root.Commands() {
		if c.Name() != "task" {
			continue
		}
		for _, sub := range c.Commands() {
			if sub.Name() != "comment" {
				continue
			}
			if f := sub.Flags().Lookup("author"); f != nil {
				t.Fatal("task comment exposes an --author flag; the author comes from the provenance header and cannot be set by the caller")
			}
			return
		}
	}
	t.Fatal("task comment command not found")
}

func TestTaskCommentBodyFromStdin(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/tasks/4/comments", http.StatusOK, `{"id":1,"author":"claude-code"}`)

	got := h.runCLIStdin(strings.NewReader("piped comment\n"), "task", "comment", "4", "--body-file", "-")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(h.nonHealthRequests()[0].body), &sent)
	if sent["body"] != "piped comment\n" {
		t.Fatalf("body = %q", sent["body"])
	}
}

func TestTaskCommentRejectsEmptyBodyLocally(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/tasks/1/comments", http.StatusOK, `{"id":1}`)

	got := h.runCLI("task", "comment", "1", "--body", "   ")
	if got.code != exitUsage {
		t.Fatalf("exit = %d, want %d", got.code, exitUsage)
	}
	if n := len(h.requests()); n != 0 {
		t.Fatalf("server saw %d requests, want 0", n)
	}
}

func TestTaskCommentRejectsBothBodySources(t *testing.T) {
	h := newHarness(t)
	got := h.runCLI("task", "comment", "1", "--body", "x", "--body-file", "/nonexistent")
	if got.code != exitUsage {
		t.Fatalf("exit = %d, want %d (stderr=%q)", got.code, exitUsage, got.stderr)
	}
	if !strings.Contains(got.stderr, "mutually exclusive") {
		t.Fatalf("stderr = %q", got.stderr)
	}
}

// TestTaskCommentWarnsOnServerTruncation: an over-long body is capped
// server-side and returned truncated with a 200, so without this the loss is
// completely silent.
func TestTaskCommentWarnsOnServerTruncation(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/tasks/8/comments", http.StatusOK, `{"id":2,"author":"claude-code","body":"abc"}`)

	got := h.runCLI("task", "comment", "8", "--body", "abcdefghij")
	if got.code != exitOK {
		t.Fatalf("exit = %d, want success — the comment WAS posted (stderr=%q)", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "TRUNCATED") {
		t.Fatalf("stderr = %q, want a truncation warning", got.stderr)
	}
	for _, want := range []string{"sent 10", "stored 3"} {
		if !strings.Contains(got.stderr, want) {
			t.Fatalf("stderr %q does not report %q — the warning must name both sizes", got.stderr, want)
		}
	}
}

// TestTaskCommentTrimsBodyBeforeComparing is the exact twin of the source test
// above — the same "compare the untrimmed thing we sent against the trimmed
// thing the server stored" mistake, one field over.
//
// A single trailing "\n" is the realistic trigger, not a corner case: it is what
// --body-file on any ordinary text file and every heredoc produce.
func TestTaskCommentTrimsBodyBeforeComparing(t *testing.T) {
	h := newHarness(t)
	// The server echoes back the TRIMMED body it stored — 100 X's, no newline.
	stored := strings.Repeat("X", 100)
	h.json("POST /api/tasks/8/comments", http.StatusOK,
		`{"id":2,"author":"claude-code","body":"`+stored+`"}`)

	// What a heredoc or --body-file sends: the same text plus a trailing newline.
	got := h.runCLI("task", "comment", "8", "--body", stored+"\n")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	if strings.Contains(got.stderr, "TRUNCATED") {
		t.Fatalf("a trailing newline produced a FALSE truncation warning: %q", got.stderr)
	}
}

// TestTaskCommentSilentWhenNotTruncated is the negative control for the warning
// above: one that fired unconditionally would satisfy the previous test while
// being worthless.
func TestTaskCommentSilentWhenNotTruncated(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/tasks/8/comments", http.StatusOK, `{"id":2,"author":"claude-code","body":"exactly"}`)

	got := h.runCLI("task", "comment", "8", "--body", "exactly")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	if strings.Contains(got.stderr, "TRUNCATED") {
		t.Fatalf("warned about truncation when nothing was truncated: %q", got.stderr)
	}
}

// TestRouteHeadersDoNotClobberTheToken is an INVARIANT GUARD, not a regression
// test — labelled as such because the difference matters.
//
// 🔴 MEASURED: moving the header loop to AFTER the Authorization block leaves
// this test green. It cannot fail today, because no caller puts an
// Authorization key in request.headers, so the ordering it describes is
// currently unobservable. It is kept to pin the property for the first caller
// that does. Do not count it as coverage of the ordering.
func TestRouteHeadersDoNotClobberTheToken(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/tasks/1/comments", http.StatusOK, `{"id":1,"author":"claude-code"}`)

	if got := h.runCLI("task", "comment", "1", "--body", "x"); got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	req := h.nonHealthRequests()[0]
	if req.auth != "Bearer "+testToken {
		t.Fatalf("Authorization = %q, want the hook token to survive the header map", req.auth)
	}
	if req.source != defaultTaskSource {
		t.Fatalf("%s = %q, want both headers present", headerSource, req.source)
	}
}
