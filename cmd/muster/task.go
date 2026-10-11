package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/taskstatus"
	"github.com/spf13/cobra"
)

// taskGetRoute is written as a template, not concatenated, so that every id
// goes through expandPath's empty-parameter guard. `task get "$TASK_ID"` with
// an unset TASK_ID must fail at exit 2 without sending a request, instead of
// asking the server for "/api/tasks/" and getting a 301.
const taskGetRoute = "/api/tasks/{id}"

// The write routes. Same template treatment as taskGetRoute: an unset shell
// variable must fail at exit 2 rather than PATCHing "/api/tasks//status", which
// net/http cleans to a different route entirely.
const (
	taskStatusRoute  = "/api/tasks/{id}/status"
	taskCommentRoute = "/api/tasks/{id}/comments"
)

// taskStatuses is the server's vocabulary, IMPORTED — not a copy.
//
// 🔴 A LOCAL LITERAL HERE WOULD BE A SECOND DEFINITION, AND THE TEST PINNING
// TWO COPIES TOGETHER IS ONE-DIRECTIONAL BY CONSTRUCTION: a status added
// server-side leaves it green while this client refuses the new value at exit 2
// against a server that accepts it. internal/taskstatus owns the single
// definition and imports nothing, so there is no second copy and nothing to pin.
var taskStatuses = taskstatus.All()

// invalidStatusMsg is this guard's OWN error text. A mutation that removes the
// check must fail with THIS string, so it cannot be mistaken for the server's
// 400 arriving instead.
const invalidStatusMsg = "refusing to send an unknown task status"

// defaultTaskSource attributes writes to the coding-agent session running the
// command. It is an allowlisted provenance value (api.taskSourceAllowlist); an
// unknown one is silently downgraded to "api" by the server, which is what
// warnAttribution exists to surface.
const defaultTaskSource = "claude-code"

// sessionIDEnvNames are the environment variables that may carry the calling
// session's id, in precedence order — first NON-EMPTY wins. They are the ONLY
// source of the session id: this CLI never invents one, never persists one and
// never accepts one as a flag, so a link can only ever record a session that
// genuinely made the call.
//
// 🔴 THE NAME IS THE WHOLE FEATURE, AND THE UPSTREAM VERSION OF THIS FILE GOT
// IT WRONG. It read a plausible variable that does not exist, so the header was
// never sent and the entire task-thread feature shipped INERT while every test
// stayed green — the tests all keyed their fake environment on the CONSTANT, so
// the constant's VALUE was never under test. TestSessionIDEnvVarNamesAreLiteral
// pins these against literal strings for exactly that reason; do not rewrite it
// to reference this variable.
//
// The second name is a DEFENSIVE ALTERNATE SPELLING rather than a guess: it
// costs one map lookup and removes a whole class of silent re-breakage if a
// build renames the primary.
var sessionIDEnvNames = []string{"CLAUDE_CODE_SESSION_ID", "CLAUDE_SESSION_ID"}

// sessionIDFrom returns the first non-empty, trimmed session id among
// sessionIDEnvNames, or "" when this is not an agent session.
func sessionIDFrom(getenv func(string) string) string {
	for _, name := range sessionIDEnvNames {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// Header names, spelled ONCE so the ledger test and the senders cannot drift.
// The server's spellings live in internal/api (taskSource, taskSessionHost);
// TestTaskProvenanceHeadersMatchTheServer pins the two together.
const (
	headerSource    = "X-Muster-Source"
	headerSessionID = "X-Muster-Session-Id"
	headerHost      = "X-Muster-Host"
)

// taskHeaders builds the provenance headers EVERY `task` subcommand sends.
//
// 🔴 THIS IS THE SEAM. The server reads X-Muster-Session-Id on every task-scoped
// route to join the session to the task's thread — and a server that reads a
// header no client sends is two components each green in isolation and broken
// TOGETHER. That is the shape TestTaskSubcommandHeaderLedger pins: it asserts
// the EXACT header set of every task subcommand and fails when the set GROWS or
// SHRINKS, so neither dropping a header nor quietly adding one can pass.
//
// 🔴 A LEDGER OF HEADER NAMES IS NOT ENOUGH, and the upstream version of this
// feature proved it: the ledger passed while the session id came from an env var
// that does not exist. The ledger pins the SET;
// TestSessionIDEnvVarNamesAreLiteral pins the VALUES the set is read from,
// against literal strings rather than these constants. Both are required — one
// alone is a test of the code against itself.
//
// An UNSET or blank session-id variable omits the header entirely rather than
// sending an empty value. The server trims and treats "" as absent either way,
// but an empty header is a CLAIM ("I am a session, and my id is nothing") where
// an absent one is the truth ("I am not a session") — and the difference shows
// up the moment something logs or counts headers rather than values.
//
// source may be empty for a route with no author to derive; it is omitted then,
// for the same reason.
func taskHeaders(getenv func(string) string, hostname func() (string, error), source string) map[string]string {
	h := map[string]string{}
	if s := strings.TrimSpace(source); s != "" {
		h[headerSource] = s
	}
	if sid := sessionIDFrom(getenv); sid != "" {
		h[headerSessionID] = sid
	}
	// Best-effort: the thread stores the host so "which machine worked this"
	// stays answerable after the transcript is reaped. A hostname lookup failure
	// is not worth failing a command over.
	if hn, err := hostname(); err == nil {
		if hn = strings.TrimSpace(hn); hn != "" {
			h[headerHost] = hn
		}
	}
	return h
}

// taskHeadersFor is the app-level wrapper: same builder, wired to this
// process's environment and hostname.
func (a *app) taskHeadersFor(source string) map[string]string {
	return taskHeaders(a.getenv, os.Hostname, source)
}

func validTaskStatus(s string) bool { return taskstatus.Valid(s) }

func newTaskCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "Durable tasks: list, read, create, set status, comment",
	}
	cmd.AddCommand(newTaskLsCmd(a), newTaskGetCmd(a), newTaskCreateCmd(a), newTaskStatusCmd(a), newTaskCommentCmd(a))
	return cmd
}

func newTaskLsCmd(a *app) *cobra.Command {
	var (
		tags     []string
		statuses []string
		limit    int
		summary  bool
	)
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "GET /api/tasks — durable tasks as JSON, filtered server-side",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			for _, t := range tags {
				q.Add("tag", t)
			}
			for _, s := range statuses {
				q.Add("status", s)
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			if summary {
				q.Set("summary", "1")
			}
			body, err := a.call(cmd.Context(), request{
				method:  http.MethodGet,
				path:    "/api/tasks",
				query:   q,
				headers: a.taskHeadersFor(defaultTaskSource),
				cred:    credHook,
			})
			if err != nil {
				return err
			}
			return a.emit(body)
		},
	}
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "filter by tag (repeatable; AND)")
	cmd.Flags().StringArrayVar(&statuses, "status", nil, "filter by status (repeatable; OR)")
	cmd.Flags().IntVar(&limit, "limit", 0, "return at most n most-recently-updated matches")
	cmd.Flags().BoolVar(&summary, "summary", false, "drop bodies/comments/attachments, emit counts instead")
	return cmd
}

func newTaskGetCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "get <id>",
		Short: "GET /api/tasks/{id} — one task, with its comments and linked agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := expandPath(taskGetRoute, map[string]string{"id": args[0]})
			if err != nil {
				return err
			}
			body, err := a.call(cmd.Context(), request{
				method:  http.MethodGet,
				path:    path,
				headers: a.taskHeadersFor(defaultTaskSource),
				cred:    credHook,
			})
			if err != nil {
				return err
			}
			return a.emit(body)
		},
	}
}

// taskCreateBody mirrors the fields handleAPITaskCreate decodes. Optional
// fields are omitempty so a minimal create posts a minimal body — an absent key
// keeps the server's pre-existing default, which is not the same as sending "".
type taskCreateBody struct {
	Directory string   `json:"directory,omitempty"`
	Title     string   `json:"title,omitempty"`
	Body      string   `json:"body"`
	Model     string   `json:"model,omitempty"`
	Repo      string   `json:"repo,omitempty"`
	Branch    string   `json:"branch,omitempty"`
	Tags      []string `json:"tags,omitempty"`
}

func newTaskCreateCmd(a *app) *cobra.Command {
	var (
		b        taskCreateBody
		bodyFile string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "POST /api/tasks — create a durable task, print {\"id\": n}",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if bodyFile != "" {
				if b.Body != "" {
					return failf(exitUsage, "--body and --body-file are mutually exclusive")
				}
				data, err := readBodyFile(bodyFile, cmd.InOrStdin())
				if err != nil {
					return err
				}
				b.Body = data
			}
			if strings.TrimSpace(b.Body) == "" {
				return failf(exitUsage, "a task body is required: pass --body or --body-file (- reads stdin)")
			}
			body, err := a.call(cmd.Context(), request{
				method:  http.MethodPost,
				path:    "/api/tasks",
				body:    b,
				headers: a.taskHeadersFor(defaultTaskSource),
				cred:    credHook,
			})
			if err != nil {
				return err
			}
			return a.emit(body)
		},
	}
	cmd.Flags().StringVar(&b.Body, "body", "", "task body (markdown)")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "read the body from a file, or - for stdin")
	cmd.Flags().StringVar(&b.Title, "title", "", "display title")
	cmd.Flags().StringVar(&b.Directory, "directory", "", "working directory the task refers to")
	cmd.Flags().StringVar(&b.Model, "model", "", "dispatch model slug")
	cmd.Flags().StringVar(&b.Repo, "repo", "", "dispatch repo")
	cmd.Flags().StringVar(&b.Branch, "branch", "", "dispatch branch")
	cmd.Flags().StringArrayVar(&b.Tags, "tag", nil, "tag (repeatable); an invalid tag is a hard 400")
	return cmd
}

type taskStatusBody struct {
	Status string `json:"status"`
}

// newTaskStatusCmd wraps PATCH /api/tasks/{id}/status — the trusted-producer
// path, which (unlike the agent self-service route) permits `complete`.
//
// 🔴 The status is validated CLIENT-SIDE before the request is built. Not for
// speed: the server's rejection is a bare "invalid or missing status", which
// does not name the vocabulary, so a typo like `in-progress` costs a round trip
// AND leaves the caller guessing. This guard names them all.
func newTaskStatusCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "status <id> <" + strings.Join(taskStatuses, "|") + ">",
		Short: "PATCH /api/tasks/{id}/status — move a task through its lifecycle",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			status := strings.TrimSpace(args[1])
			if !validTaskStatus(status) {
				return failf(exitUsage,
					"%s: %q. Valid statuses are %s (there is no `dismissed` — a dismissed task is soft-deleted and hidden from the board, row and comment thread intact, not moved to a status).",
					invalidStatusMsg, args[1], strings.Join(taskStatuses, ", "))
			}
			path, err := expandPath(taskStatusRoute, map[string]string{"id": args[0]})
			if err != nil {
				return err
			}
			body, err := a.call(cmd.Context(), request{
				method:  http.MethodPatch,
				path:    path,
				body:    taskStatusBody{Status: status},
				headers: a.taskHeadersFor(defaultTaskSource),
				cred:    credHook,
			})
			if err != nil {
				return err
			}
			return a.emit(body)
		},
	}
}

type taskCommentBody struct {
	Body string `json:"body"`
}

// newTaskCommentCmd wraps POST /api/tasks/{id}/comments.
//
// The author comes from the provenance header and NEVER from the body — that is
// the server's impersonation guard, so this command has no --author flag by
// construction. An unrecognised source is not an error server-side: it is
// silently attributed to "api", which warnAttribution turns into a visible
// stderr note by comparing what we asked for against what came back.
func newTaskCommentCmd(a *app) *cobra.Command {
	var (
		b        taskCommentBody
		bodyFile string
		source   string
	)
	cmd := &cobra.Command{
		Use:   "comment <id>",
		Short: "POST /api/tasks/{id}/comments — post a comment, authored from " + headerSource,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if bodyFile != "" {
				if b.Body != "" {
					return failf(exitUsage, "--body and --body-file are mutually exclusive")
				}
				data, err := readBodyFile(bodyFile, cmd.InOrStdin())
				if err != nil {
					return err
				}
				b.Body = data
			}
			if strings.TrimSpace(b.Body) == "" {
				return failf(exitUsage, "a comment body is required: pass --body or --body-file (- reads stdin)")
			}
			// 🔴 Trim before BOTH the header and the comparison. The server trims
			// the header itself, so comparing an untrimmed source against the
			// trimmed author it returns accuses a CORRECT call of being downgraded.
			source = strings.TrimSpace(source)
			path, err := expandPath(taskCommentRoute, map[string]string{"id": args[0]})
			if err != nil {
				return err
			}
			raw, err := a.call(cmd.Context(), request{
				method:  http.MethodPost,
				path:    path,
				body:    b,
				headers: a.taskHeadersFor(source),
				cred:    credHook,
			})
			if err != nil {
				return err
			}
			a.warnAttribution(source, raw)
			a.warnTruncated(b.Body, raw)
			return a.emit(raw)
		},
	}
	cmd.Flags().StringVar(&b.Body, "body", "", "comment body (markdown)")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "read the body from a file, or - for stdin")
	cmd.Flags().StringVar(&source, "source", defaultTaskSource,
		"provenance sent as "+headerSource+"; the server derives the author from it")
	return cmd
}

// warnAttribution reports, on stderr, a source the server did not accept.
//
// The check is made against the RESPONSE rather than a client-side allowlist on
// purpose: the allowlist lives in internal/api and is unexported, so any copy
// here would be a second place to keep in sync. Asking the server what it
// actually recorded cannot drift.
func (a *app) warnAttribution(requested string, raw []byte) {
	var c struct {
		Author string `json:"author"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Author == "" || c.Author == requested {
		return
	}
	fmt.Fprintf(a.stderr,
		"note: requested source %q but the comment was authored as %q — %q is not in muster's provenance allowlist, so the attribution was silently downgraded\n",
		requested, c.Author, requested)
}

// warnTruncated reports, on stderr, a body the server stored in shortened form.
//
// The server caps a comment at a fixed rune count and returns the STORED text
// with a 200, so an over-long body — realistically a piped report via
// --body-file — is silently clipped and the command still succeeds. Like
// warnAttribution this compares what came back against what was sent rather
// than duplicating the server's limit, so the cap can change without this going
// stale.
//
// 🔴 COMPARE THE TRIMMED BODY. The server stores strings.TrimSpace(body.Body),
// so surrounding whitespace is NORMALISED, not truncated — nothing is lost.
// Comparing the raw sent string against the stored one therefore accuses every
// ordinary heredoc and --body-file post (both end in "\n") of losing exactly one
// rune, and tells the operator it is "not recoverable". That is the same defect
// warnAttribution had for the source header, fixed the same way: mirror the ONE
// normalisation the server documents, and nothing else, so a real truncation
// still warns.
func (a *app) warnTruncated(sent string, raw []byte) {
	var c struct {
		Body string `json:"body"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return
	}
	stored, want := len([]rune(c.Body)), len([]rune(strings.TrimSpace(sent)))
	if stored >= want {
		return
	}
	fmt.Fprintf(a.stderr,
		"note: comment body was TRUNCATED by the server — sent %d runes, stored %d. The command still succeeded; the missing %d runes are not recoverable from muster.\n",
		want, stored, want-stored)
}

func readBodyFile(path string, stdin io.Reader) (string, error) {
	if path == "-" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", wrapf(exitUsage, err, "reading body from stdin")
		}
		return string(data), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", wrapf(exitUsage, err, "reading %s", path)
	}
	return string(data), nil
}
