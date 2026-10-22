package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/credentials"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/devices"
	"github.com/ZacxDev/muster/internal/flows"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/pins"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/recents"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/sse"
	"github.com/ZacxDev/muster/internal/uploads"
	"github.com/ZacxDev/muster/internal/uploads/uploadstest"
	"github.com/ZacxDev/muster/internal/users"
)

// ---------------------------------------------------------------------------
// THE ISOLATION WALK (accounts plan §4; PR 3c). This is the gate that says a
// second user cannot see or change the first user's data THROUGH ANY ROUTE.
//
// Row-level security (3a) is the mechanism; this test is the proof, and it is
// deliberately not a list of spot checks. It reads routes.golden, demands a
// CLASS for every route in it (an unclassified route fails — a new route must
// say what it is before it ships), fills every path parameter with USER A's
// ids, and drives each route as USER B through each of B's credentials: B's
// browser session, B's agent's own token on the main port, and the same token
// on the dedicated connect port (OnlyConnectSurface). It then asserts:
//
//   - no response carries A's CANARY strings (seeded into every object A owns:
//     task title/body, comment, attachment name, agent display name and chat,
//     connector auth username, grant reason, GitHub login) — and, on routes
//     that take no A id, not A's identifiers either (agent and connector name);
//   - a route that names A's object answers no 2xx (403/404/401/...);
//   - the SSE leg: B's open /events stream receives nothing while A writes;
//   - the mutation leg: every row A owns is byte-identical after B's walk —
//     B's writes against A's ids changed nothing;
//   - the connect leg: B's agent never gets A's secret injected upstream,
//     even holding an APPROVED grant for A's connector's NAME (#61's note: the
//     grant→connector link is a name, not a foreign key);
//   - nothing B (a member) did reached the operator's router (push / mirror),
//     and B's agent cannot file approval cards (503 → in-terminal prompt).
//
// The ADMIN leg repeats the param and list checks as user 1 — the operator,
// through its session AND the shared machine (hook) token — against the
// MEMBER A's objects: an admin's ordinary routes are scoped to its own rows
// too ("see all" is for /admin/* only, and there is none yet).
//
// 🔴 THE NEGATIVE CONTROL IS THE SAME WALK AS A. Every request B made against
// A's ids is re-sent as A, identically except for the identity, and must NOT
// be refused the way B was (or must show A's canary). Without it a refusal
// could be a malformed request — a 404 for a wrong path is indistinguishable
// from a 404 for "not yours". The control is what makes each refusal a fact
// about identity. And the list routes that render data must show A its canary
// (wantCanaryForA), so "B saw no canary" is a fact about B, not about a
// renderer that shows nobody anything.
// ---------------------------------------------------------------------------

// routeClass is what a route IS, for the walk.
type routeClass string

const (
	classPublic     routeClass = "public"      // no credential; must leak nothing
	classList       routeClass = "list"        // a session route naming no object
	classParam      routeClass = "param"       // a session route naming an object by id/name
	classAdmin      routeClass = "admin"       // the shared machine (hook/service) tier: acts as user 1
	classAgentToken routeClass = "agent-token" // an agent's own token: /agent/task, /agent/approvals, ...
	classConnect    routeClass = "connect"     // the connector surface (also served on the connect port)
	classMirror     routeClass = "mirror"      // the mirrored terminal (#63), addressed by ?host=&session=
	classAPIToken   routeClass = "api-token"   // a user's own PERSONAL token only: the token API (accounts PR 6)
	// classAdminOnly is a SESSION route served to role=admin only — the admin
	// pages (/admin/*, accounts PR 5), the one place "see all" exists. It is
	// NOT classAdmin: that class is the shared MACHINE tier acting as user 1.
	// A member must be refused every one (TestAMemberCannotReachAnyAdminRoute,
	// which also holds the admin's control); the generic walk drives them as
	// B and A, and keeps them out of the admin leg, whose writes would act on A.
	classAdminOnly routeClass = "admin-only"
	// classRunner is a browser-flow RUNNER token's door (/runner/flows/*,
	// 0012): no session, agent, hook or personal credential opens it, and a
	// runner token acts as its owner — so B's runner must reach nothing of A's.
	classRunner routeClass = "runner"
	// classPlugin is a PLUGIN token's door (/plugin/v1/*, 0015): no other
	// credential opens it, and a plugin token acts as its owner — so B's plugin
	// must reach nothing of A's.
	classPlugin routeClass = "plugin"
	// classMirrorHost is a mirror HOST token's MCP door (POST /mirror/mcp): no
	// other credential opens it, and it names no object in its path — a session
	// is claimed by header and checked against the token's own host
	// (mirror_mcp_pg_test.go walks that refusal across hosts and owners).
	classMirrorHost routeClass = "mirror-host"
	// classDevice is a paired DEVICE credential's tier (/api/device/*, 0039): no
	// other credential opens it, it acts as the device's owner, and it names no
	// object — a device acts on itself — so B's device must reach nothing of A's.
	classDevice routeClass = "device"
)

// isolationClasses classifies EVERY route in routes.golden. Exhaustive both
// ways (TestEveryRouteIsClassifiedForIsolation).
var isolationClasses = map[string]routeClass{
	"GET /apple-touch-icon-precomposed.png": classPublic,
	"GET /apple-touch-icon.png":             classPublic,
	"GET /favicon.ico":                      classPublic,
	"GET /health":                           classPublic,
	"GET /login":                            classPublic,
	"GET /manifest.webmanifest":             classPublic,
	"GET /metrics":                          classPublic,
	"GET /readyz":                           classPublic,
	"GET /static/":                          classPublic,
	"GET /sw.js":                            classPublic,
	"POST /login":                           classPublic,
	"POST /logout":                          classPublic,
	// accounts PR 4: the admin claim (token in a form field) and the account page.
	"GET /auth/claim":  classPublic,
	"POST /auth/claim": classPublic,
	"GET /account":     classList,
	// The step-up confirm names no object: it re-proves the CALLER (stepup.go).
	"POST /auth/confirm": classList,
	// Its passkey form names no object either: the options are for the caller's
	// own passkeys, and verify refuses an assertion that is not the caller's.
	"POST /auth/confirm/passkey/options": classList,
	"POST /auth/confirm/passkey/verify":  classList,
	// Bring-your-own credentials: the caller's own page and writes (list), and
	// the delete that names an account (param, by NAME — A's is the canary).
	"GET /account/credentials":                       classList,
	"POST /account/credentials/claude":               classList,
	"POST /account/credentials/openrouter":           classList,
	"POST /account/credentials/openrouter/delete":    classList,
	"POST /account/credentials/claude/{name}/delete": classParam,

	// Personal API tokens (accounts PR 6). The page lists the CALLER's tokens
	// (A's carry the canary in their names); revoke names a token by id.
	"GET /account/tokens":              classList,
	"POST /account/tokens":             classList,
	"POST /account/tokens/{id}/revoke": classParam,
	// Passkeys. The sign-in pair is public (like POST /login). The account
	// routes act on the caller's OWN passkeys at GoTrue with the caller's own
	// access token, so GoTrue — not muster's database — scopes them; delete
	// names a passkey by id (param: A's passkey id).
	"POST /auth/passkey/options":         classPublic,
	"POST /auth/passkey/verify":          classPublic,
	"GET /account/passkeys":              classList,
	"POST /account/passkeys/options":     classList,
	"POST /account/passkeys/verify":      classList,
	"POST /account/passkeys/{id}/delete": classParam,
	"GET /api/whoami":                    classAdmin,
	"GET /api/tokens":                    classAPIToken,
	"DELETE /api/tokens/{id}":            classAPIToken,
	// muster-native Web Push (migration 0013). The key and the subscribe /
	// unsubscribe writes act on the CALLER's own devices (list); the account
	// page's remove names a device by id (param, A's is the object). A's device
	// carries the canary in its user agent and its endpoint.
	// Paired devices (0039): minting a code is the caller's own; removing
	// names a device; pairing is public (the code is the authority); the
	// device tier is the device credential's alone.
	"POST /account/devices/pair":        classList,
	"POST /account/devices/{id}/remove": classParam,
	"POST /api/device/pair":             classPublic,
	"DELETE /api/device":                classDevice,
	"GET /api/device/config":            classDevice,
	"PUT /api/device/push":              classDevice,
	"DELETE /api/device/push":           classDevice,
	"GET /api/push/vapid-public-key":    classList,
	"POST /api/push/subscribe":          classList,
	"POST /api/push/unsubscribe":        classList,
	"POST /account/push/test":           classList,
	"POST /account/push/{id}/remove":    classParam,
	"POST /account/push/preferences":    classList,
	// accounts PR 5: invite redemption and the password reset (tokens in form
	// fields, like the claim), and the admin pages.
	"GET /invite":      classPublic,
	"POST /invite":     classPublic,
	"GET /auth/reset":  classPublic,
	"POST /auth/reset": classPublic,

	"GET /admin/invites":                     classAdminOnly,
	"GET /admin/users":                       classAdminOnly,
	"POST /admin/invites":                    classAdminOnly,
	"POST /admin/invites/{id}/revoke":        classAdminOnly,
	"POST /admin/users/{id}/disable":         classAdminOnly,
	"POST /admin/users/{id}/enable":          classAdminOnly,
	"POST /admin/users/{id}/reset-link":      classAdminOnly,
	"POST /admin/users/{id}/role":            classAdminOnly,
	"POST /admin/users/{id}/passkeys/delete": classAdminOnly,

	"GET /{$}":                     classList,
	"GET /agents":                  classList,
	"GET /api/directories":         classList,
	"GET /api/openrouter/models":   classList,
	"GET /connectors":              classList,
	"GET /events":                  classList,
	"GET /github/callback":         classList,
	"GET /github/connect":          classList,
	"GET /repos":                   classList,
	"GET /share":                   classList,
	"GET /tasks":                   classList,
	"GET /ui/agents":               classList,
	"GET /ui/agents/new":           classList,
	"GET /ui/agents/notes":         classList,
	"GET /ui/agents/repos":         classList,
	"GET /ui/chief/panel":          classList,
	"GET /ui/chief/threads/search": classList,
	"GET /ui/connector-requests":   classList,
	"GET /ui/connectors":           classList,
	"GET /ui/connectors/new":       classList,
	"GET /ui/notifications":        classList,
	"GET /ui/repos":                classList,
	"GET /ui/tags":                 classList,
	"GET /ui/tasks":                classList,
	"GET /ui/tasks/new":            classList,
	"GET /ui/tasks/review-count":   classList,
	"POST /agents":                 classList, // the form names A's task as note_id
	"POST /chief/provision":        classList,
	"POST /connectors":             classList,
	"POST /github/disconnect":      classList,
	"POST /tasks":                  classList,
	"POST /tasks/merge":            classList, // the form names A's task as the loser

	"DELETE /agents/{id}":                      classParam,
	"DELETE /connector-grants/{id}":            classParam,
	"DELETE /connectors/{name}":                classParam,
	"DELETE /tasks/{id}":                       classParam,
	"DELETE /tasks/{id}/comments/{cid}":        classParam,
	"DELETE /tasks/{id}/tags/{tag}":            classParam,
	"GET /agents/{name}":                       classParam,
	"GET /agents/{name}/logs/stream":           classParam,
	"GET /agents/{name}/terminal":              classParam,
	"GET /agents/{name}/terminal/ws":           classParam,
	"GET /agents/{name}/ws":                    classParam,
	"GET /tasks/{id}":                          classParam,
	"GET /tasks/{id}/attachments/{aid}":        classParam,
	"GET /ui/agents/{id}/card":                 classParam,
	"GET /ui/focus/history":                    classParam,
	"GET /ui/agents/{id}/rename":               classParam,
	"GET /ui/agents/{id}/status":               classParam,
	"GET /ui/agents/{name}/chat-log":           classParam,
	"GET /ui/agents/{name}/task":               classParam,
	"GET /ui/connectors/{name}/rotate":         classParam,
	"GET /ui/tasks/{id}/card":                  classParam,
	"GET /ui/tasks/{id}/edit":                  classParam,
	"PATCH /tasks/{id}/status":                 classParam,
	"POST /agents/sessions/{sid}/read":         classParam,
	"POST /agents/{id}/model":                  classParam,
	"POST /agents/{id}/name":                   classParam,
	"POST /agents/{id}/sessions":               classParam,
	"POST /agents/{id}/sessions/{sid}/default": classParam,
	"POST /agents/{id}/start":                  classParam,
	"POST /agents/{id}/stop":                   classParam,
	"POST /connectors/{name}/rotate":           classParam,
	"POST /tasks/{id}/comments":                classParam,
	"POST /tasks/{id}/edit":                    classParam,
	"POST /tasks/{id}/complete":                classParam,
	"POST /tasks/{id}/tags":                    classParam,
	"POST /ui/connector-requests/{id}/approve": classParam,
	"POST /ui/connector-requests/{id}/deny":    classParam,

	"DELETE /api/tasks/{id}":                classAdmin,
	"DELETE /api/tasks/{id}/comments/{cid}": classAdmin,
	"GET /api/agents":                       classAdmin,
	"GET /api/agents/{name}/messages":       classAdmin,
	"GET /api/projects":                     classAdmin,
	"GET /api/sessions/{id}/tasks":          classAdmin,
	"GET /api/tags":                         classAdmin,
	"GET /api/tasks":                        classAdmin,
	"GET /api/tasks/{id}":                   classAdmin,
	"PATCH /api/tasks/{id}":                 classAdmin,
	"PATCH /api/tasks/{id}/status":          classAdmin,
	"POST /api/agents/{name}/messages":      classAdmin,
	// Chief replacement (chief_replace.go), hook tier, acting as the caller.
	"POST /api/chief/replace": classAdmin,
	"POST /api/tasks":         classAdmin,
	// Session routing (0021): a mirrored session's answer, on the hook tier
	// (requireArmedHookToken), acting as the route's owner.
	"POST /api/routes/{id}/reply": classAdmin,
	// Retrying an unconfirmed plugin message (plugin_retry.go), hook tier, as its owner.
	"POST /api/plugins/messages/{id}/retry": classAdmin,
	"POST /api/tasks/{id}/comments":         classAdmin,
	// The machine job API (jobs.go): the service tier, acting as user 1.
	"GET /api/jobs/{id}": classAdmin,
	"POST /api/jobs":     classAdmin,

	"DELETE /agent/approvals/{id}": classAgentToken,
	"GET /agent/approvals/{id}":    classAgentToken,
	"GET /agent/muster":            classAgentToken,
	"GET /agent/task":              classAgentToken,
	"PATCH /agent/task/status":     classAgentToken,
	"POST /agent/approvals":        classAgentToken,
	"POST /agent/blocked":          classAgentToken,
	"POST /agent/work":             classAgentToken,
	"POST /agent/task/comment":     classAgentToken,

	"/connect/{connector}/{host}/{path...}": classConnect,
	"GET /agent/connectors":                 classConnect,
	"GET /agent/connectors/{name}":          classConnect,
	"POST /agent/connectors/{name}/request": classConnect,
	"POST /mcp":                             classConnect,

	// The mirrored terminal names its object in the QUERY (?host=&session=),
	// and fill() puts A's mirrored host there. The tunnel door is public to
	// the principal walk (only a mirror token opens it): B's credentials must
	// open nothing on it and leak nothing.
	"GET /mirror/connect": classPublic,
	// The AskUserQuestion hook's door: like the tunnel, a mirror token only.
	"POST /mirror/ask":        classPublic,
	"POST /mirror/mcp":        classMirrorHost,
	"GET /mirror/terminal":    classMirror,
	"GET /mirror/terminal/ws": classMirror,

	// Browser flows (0012). The page and its forms are the session tier; the
	// JSON API is a personal token only; the runner door is a runner token
	// only. Each names A's job (or A's runner token) by id.
	"GET /flows":                            classList,
	"POST /flows/jobs":                      classList,
	"POST /flows/runner-tokens":             classList,
	"POST /flows/jobs/{id}/cancel":          classParam,
	"POST /flows/runner-tokens/{id}/revoke": classParam,
	// An agent's request is decided by its owner only (PR 4).
	"POST /flows/requests/{id}/approve": classParam,
	"POST /flows/requests/{id}/deny":    classParam,
	"GET /api/flows/jobs":               classAPIToken,
	"GET /api/flows/jobs/{id}":          classAPIToken,
	"POST /api/flows/jobs":              classAPIToken,
	"POST /api/flows/jobs/{id}/cancel":  classAPIToken,
	"POST /runner/flows/claim":          classRunner,
	"POST /runner/flows/{id}/heartbeat": classRunner,
	"POST /runner/flows/{id}/result":    classRunner,

	// Plugins (0015). The page and its forms are the session tier and the
	// revoke names A's plugin by id; the message route is a plugin token only
	// and names no object (a message lands in the token owner's own rows).
	"GET /plugins":              classList,
	"POST /plugins":             classList,
	"POST /plugins/{id}/revoke": classParam,
	// Pins (0017): the form names A's agent as agent_id. B's pin of it is
	// refused (the owner trigger) and B's unpin deletes only B's rows; A's
	// seeded pins are in the snapshot, so either reaching A's would show.
	"POST /agents/pins":        classList,
	"POST /agents/pins/delete": classList,
	// The send queue (0019): the queue form, the list and the plain confirm
	// page name A's mirrored window; Cancel, Retry and Delete name A's queued
	// message by id.
	// Each must answer B exactly as for a ghost, and A's queued message (its
	// body carries the canary) is in the snapshot.
	"GET /send-queue":              classMirror,
	"POST /send-queue":             classMirror,
	"POST /send-queue/{id}/cancel": classParam,
	"POST /send-queue/{id}/retry":  classParam,
	// Delete of a finished message (A's seeded message is queued, so A's own
	// control is refused 409 and nothing of A's is removed; B's is 404).
	"POST /send-queue/{id}/delete": classParam,
	// The Retry's plain confirm page form (a stale Retry with no script).
	"POST /send-queue/{id}/retry/confirm": classParam,
	// Send now of a queued message, and its plain confirm page's form.
	"POST /send-queue/{id}/send-now":         classParam,
	"POST /send-queue/{id}/send-now/confirm": classParam,
	// Edit of a queued message (it holds A's message), its Save and Cancel,
	// and the plain confirm pages' forms for a stale Edit and Save.
	"POST /send-queue/{id}/edit":              classParam,
	"POST /send-queue/{id}/edit/confirm":      classParam,
	"POST /send-queue/{id}/edit/save":         classParam,
	"POST /send-queue/{id}/edit/save/confirm": classParam,
	"POST /send-queue/{id}/edit/cancel":       classParam,
	"POST /send-queue/confirm":                classMirror,
	"POST /plugin/v1/messages":                classPlugin,
	// Reply drafts (PR C): the owner's decisions name A's draft by id; the
	// plugin's claim names nothing (it leases the token owner's own drafts) and
	// its result names A's draft — which B's plugin must not settle.
	"POST /plugins/drafts/{id}/approve":  classParam,
	"POST /plugins/drafts/{id}/reject":   classParam,
	"POST /plugins/messages/{id}/retry":  classParam,
	"POST /plugin/v1/drafts/claim":       classPlugin,
	"POST /plugin/v1/drafts/{id}/result": classPlugin,
	// The plugin's keyed task (plugin_tasks.go): it names a key, never an id;
	// B's plugin finds only B's tasks by it (row-level security).
	"POST /plugin/v1/tasks": classPlugin,

	// The mirrored rendered view (tunnel v2) addresses its session like the
	// mirrored terminal above (?host=&session=); the
	// send carries host and session in its FORM, which fill() also sets.
	"GET /mirror/view":       classMirror,
	"GET /mirror/view/ws":    classMirror,
	"POST /mirror/view/send": classMirror,
	// The Terminal view's composer: the same handler, the same form fields.
	"POST /mirror/terminal/send": classMirror,
	// Drag-to-scroll: the same address fields, plus `lines`.
	"POST /mirror/terminal/scroll": classMirror,
	// A send that came back from a sign-in as a GET: the address is in the query.
	"GET /mirror/view/send":     classMirror,
	"GET /mirror/terminal/send": classMirror,
	// The plain confirm page's form: the same address fields, plus `kind`.
	"POST /mirror/confirm-send": classMirror,
	// AskUserQuestion (mirror_ask.go): the live region (address in the query)
	// and the answers (address in the form, plus the call's tool id).
	"GET /mirror/view/asks": classMirror,
	"POST /mirror/view/ask": classMirror,
	// A Hermes approval's answer (attention_hermes.go): addressed by the
	// window's host and session in the form, like a send.
	"POST /attention/hermes/approve": classMirror,
	// The Rendered view composer's `/` and `:` menu data: the same address in
	// the query; B asking for A's window's catalog must 404 before the plugin.
	"GET /mirror/view/complete": classMirror,
	// An opened subagent card's steps: the same address in the query, plus
	// the agent id.
	"GET /mirror/view/subagent": classMirror,
	// A message to an agent as a form post, and the GET it can come back as.
	"POST /agents/{name}/send": classParam,
	"GET /agents/{name}/send":  classParam,
	// The attention queue (0025). The page, its live list and the badge name
	// no object (each reads the caller's own queue); the answer and dismiss
	// forms name A's ask by id.
	"GET /attention":                   classList,
	"GET /attention/items":             classList,
	"GET /attention/badge":             classList,
	"POST /attention/asks/{id}/answer": classParam,
	"POST /attention/asks/{id}/cancel": classParam,
	// An unconfirmed answer delivery's Retry (migration 0031): one of the
	// caller's asks by id, under row-level security.
	"POST /attention/asks/{id}/redeliver": classParam,
	// Dictation (transcribe.go) names no object: it turns the caller's recording
	// into text for the caller and reads or writes no row.
	"POST /transcribe": classList,
	// File attachments (uploads.go). The upload names no object (it creates the
	// caller's own); the owner's view and the revoke name A's upload by its
	// public id; the signed link is public — its HMAC is the credential, which
	// no walker holds (sideEffectControlled).
	"POST /uploads":             classList,
	"GET /uploads/{id}/file":    classParam,
	"POST /uploads/{id}/revoke": classParam,
	"GET /f/{id}":               classPublic,
}

// wantCanaryForA: list routes that render A's data to A. The negative control
// demands A sees its canary (or an identifier) on each, so B's clean body on
// the same route is a fact about B.
var wantCanaryForA = []string{
	"GET /ui/agents", "GET /ui/agents/notes", "GET /ui/connector-requests", "GET /ui/connectors",
	"GET /ui/repos", "GET /ui/tasks", "GET /ui/chief/panel",
	// The AGGREGATES — vocabularies, suggestion lists, unread rolls — read
	// across rows rather than by id, which is exactly where a missing scope
	// hides (plan §Recon 4). A's tag and project are identities, its unread
	// assistant message names its agent.
	// (/ui/tasks/new and /ui/agents/new carry no per-user aggregate: the
	// first lists the router's directories, admin-only and checked by the
	// operator canary; the second is the bare dispatch form.)
	"GET /ui/tags", "GET /ui/notifications",
	// The caller's own credentials: A's Claude account is NAMED with the canary,
	// so it shows on A's credentials page and on A's dispatch form's account
	// pin (which lists the caller's own accounts since bring-your-own).
	"GET /account/credentials", "GET /ui/agents/new",
	// Accounts PR 6: A's tokens are canary-named, so the token page and the
	// token API must show A its own — and a machine-tier list read through A's
	// personal token must show A's tasks (the hook tier had no such control
	// before: only the operator held a machine credential).
	"GET /account/tokens", "GET /api/tokens", "GET /api/tasks",
	// Browser flows: A's jobs carry the canary in a parameter, so A's page, A's
	// token list and A's runner's claim (of A's second job) each show it.
	"GET /flows", "GET /api/flows/jobs", "POST /runner/flows/claim",
	// Web Push: the account page lists the caller's devices, and A's carries
	// the canary in its user agent.
	"GET /account",
	// Plugins: A's second plugin is canary-labelled, so A's page shows it.
	"GET /plugins",
}

func TestEveryRouteIsClassifiedForIsolation(t *testing.T) {
	golden := map[string]bool{}
	for _, r := range readGolden(t) {
		golden[r] = true
	}
	var unclassified, stale []string
	for r := range golden {
		if _, ok := isolationClasses[r]; !ok {
			unclassified = append(unclassified, r)
		}
	}
	for r := range isolationClasses {
		if !golden[r] {
			stale = append(stale, r)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(stale)
	if len(unclassified) > 0 {
		t.Errorf("routes in %s with no isolation class (public|list|param|admin|admin-only|agent-token|connect|mirror|api-token|runner):\n  %s",
			goldenPath, strings.Join(unclassified, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("classified routes not in %s:\n  %s", goldenPath, strings.Join(stale, "\n  "))
	}
	// The public allowlist is principal_test.go's: one list, two tests.
	for _, r := range publicRoutes {
		if isolationClasses[r] != classPublic {
			t.Errorf("%s is public for the principal walk but %q here", r, isolationClasses[r])
		}
	}
}

// --- the world ---------------------------------------------------------------

const (
	isoHookToken    = "iso-walk-hook-token-0123456789abcdef"
	isoServiceToken = "iso-walk-service-token-0123456789abcd"
	isoPassword     = "iso-walk-operator-password"
	isoUserHeader   = "X-Iso-Test-User"
)

type isoUser struct {
	name  string
	uid   int64
	token string // the user's agent's own token
	agent agents.Agent
	task  notes.Note
}

type isoWorld struct {
	// uploads and uploadSvc are the file attachments' object store and service
	// (0024): A's upload's bytes live in the former.
	uploads   *uploadstest.Objects
	uploadSvc *uploads.Service

	t        *testing.T
	pool     *pgxpool.Pool
	srv      *Server
	h        http.Handler
	connectH http.Handler
	bus      *sse.Broadcaster
	sys      context.Context
	a, b, op isoUser
	suffix   string
	// A's canary strings (data) and identifiers.
	canary      string
	aIdentities []string
	opCanary    string
	// A's ids, by path placeholder.
	aTask2       notes.Note
	aComment     notes.Comment
	aAttachment  notes.Attachment
	aTag         string
	aChatSession agents.ChatSession
	aConnName    string
	aGrantID     int64
	aSessionLink string
	aChief       agents.Agent
	opApproval   string
	aProject     string
	objA, ghost  isoObjs
	creds        *credentials.PGStore
	upHost       string
	up           *isoUpstream
	router       *isoRouter
	prov         *isoProvisioner
	// gt is the fake GoTrue the admin pages talk to (accounts PR 5); opInvite
	// is a pending invite the operator created.
	gt       *gotruetest.Fake
	opInvite users.Invite
	// The REAL gotrue-session leg (accounts PR 4): a second server in gotrue
	// mode over the SAME stores, signing users in through the fake GoTrue
	// issuer — no sessionUserOverride. gtTokens is each user's minted pair.
	gtH      http.Handler
	gtFake   *gotruetest.Fake
	gtTokens map[int64][2]string
	aSub     string // A's GoTrue subject in the twin's fake
	bSub     string // B's
	// Personal API tokens (accounts PR 6): each user's own credential, and A's
	// SECOND token — the object the token routes name (so revoking it never
	// revokes the credential A's control walk is using).
	apiTokens map[int64]string
	aToken2   apitokens.Token
	aTokTask  notes.Note // A's task (with aTokComment) for A's personal-token control
	// Web Push: the recording pusher every send goes through, and A's device
	// (its endpoint carries the canary).
	pusher      *isoPusher
	aEndpoint   string
	aTokComment notes.Comment // see TestNoRouteLetsOneUserReachAnothersData's objTok
	// Browser flows (0012): each user's runner token (for the SAME host, so
	// B's claim is a real attempt at A's queue), A's two queued jobs (the
	// first is the object the {id} routes name; the second is what A's own
	// runner claims), and A's SECOND runner token (the revoke route's object,
	// so revoking it never revokes the credential A's runner control uses).
	runnerTokens map[int64]string
	flowHost     string
	aJob, aJob2  flows.Job
	aRunner2     flows.RunnerToken
	aFlowReq     flows.FlowRequest // A's agent's pending browser-flow request (PR 4)
	// Plugins (0015): each user's plugin token, and A's SECOND plugin
	// (canary-labelled) — the revoke route's object, so revoking it never
	// revokes a credential.
	pluginTokens map[int64]string
	// Paired devices (0039): each user's device credential, and A's device
	// (canary-named), the object Remove names.
	deviceTokens map[int64]string
	aDevice      devices.Device
	aPlugin2     plugins.Plugin
	aDraft       plugins.Draft
}

func newIsoWorld(t *testing.T) *isoWorld {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	w := &isoWorld{t: t, pool: pool, sys: authz.System(ctx)}
	w.suffix = strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	w.canary = "cnrya" + w.suffix
	w.opCanary = "cnryop" + w.suffix

	mkUser := func(name, role string) isoUser {
		var id int64
		if err := pool.QueryRow(w.sys, `INSERT INTO users (role, display_name) VALUES ($1, $2) RETURNING id`, role, name+w.suffix).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return isoUser{name: name, uid: id}
	}
	w.a = mkUser("a", "member")
	w.b = mkUser("b", "member")
	w.op = isoUser{name: "op", uid: authz.OperatorUserID}

	ns := notes.NewPG(pool)
	as := agents.NewPG(pool)
	cs, err := connectors.NewPG(pool, bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	gh, err := github.NewPG(pool, bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	as2 := func(u isoUser) context.Context {
		role := authz.RoleMember
		if u.uid == authz.OperatorUserID {
			role = authz.RoleAdmin
		}
		return authz.WithPrincipal(ctx, authz.Principal{UserID: u.uid, Role: role, Via: authz.ViaSession})
	}
	must := func(err error, what string) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
	}

	// Every user: one task and one agent (with its own token) on it.
	for _, u := range []*isoUser{&w.a, &w.b, &w.op} {
		uc := as2(*u)
		title, body := u.name+"-task-"+w.suffix, u.name+" body"
		if u == &w.a {
			title, body = w.canary+" title", w.canary+" body"
		}
		if u == &w.op {
			title = w.opCanary + " title"
		}
		nt := notes.Note{Title: title, Body: body, Status: notes.StatusOpen}
		if u == &w.a {
			nt.Repo, nt.Directory = w.canary+"/repo", "/srv/"+w.canary
		}
		u.task, err = ns.Create(uc, nt)
		must(err, u.name+" task")
		u.token = "iso-" + u.name + "-agent-token-" + w.suffix
		display := u.name + " agent"
		if u == &w.a {
			display = w.canary + " agent"
		}
		u.agent, err = as.Create(uc, agents.Agent{Name: "iso" + u.name + w.suffix, DisplayName: display,
			Namespace: "ns-iso" + u.name + w.suffix, HooksToken: u.token, NoteID: &u.task.ID, Status: agents.StatusRunning})
		must(err, u.name+" agent")
	}

	// A's every other object, each carrying the canary.
	ua := as2(w.a)
	// A's second task is READY FOR REVIEW: the review-count aggregate.
	w.aTask2, err = ns.Create(ua, notes.Note{Title: w.canary + " second", Status: notes.StatusReadyForReview})
	must(err, "a task2")
	_, err = ns.SetStatus(ua, w.aTask2.ID, notes.StatusReadyForReview) // Create files every task open
	must(err, "a task2 review")
	w.aComment, err = ns.AddComment(ua, notes.Comment{NoteID: w.a.task.ID, Author: "a", Body: w.canary + " comment"})
	must(err, "a comment")
	w.aAttachment, err = ns.AddAttachment(ua, w.a.task.ID, notes.Attachment{Filename: w.canary + ".txt",
		ContentType: "text/plain", SizeBytes: int64(len(w.canary)), Data: []byte(w.canary)})
	must(err, "a attachment")
	w.aTag = "isotag" + w.suffix
	w.aProject = "isoproj" + w.suffix
	_, err = ns.AddTags(ua, w.a.task.ID, []string{w.aTag, "project:" + w.aProject})
	must(err, "a tag")
	w.aSessionLink = "isosess-" + w.suffix
	_, _, err = ns.LinkSession(ua, w.a.task.ID, notes.SessionLink{SessionID: w.aSessionLink, Role: "created", Project: w.canary}, false)
	must(err, "a session link")
	w.aChatSession, err = as.LatestOrCreateSession(ua, w.a.agent.ID, w.a.agent.Name)
	must(err, "a chat session")
	_, err = as.AddChatMessage(ua, agents.ChatMessage{AgentID: w.a.agent.ID, SessionID: w.aChatSession.ID, Role: "user", Content: w.canary + " chat"})
	must(err, "a chat message")
	// An UNREAD assistant reply: the notifications roll-up.
	_, err = as.AddChatMessage(ua, agents.ChatMessage{AgentID: w.a.agent.ID, SessionID: w.aChatSession.ID, Role: "assistant", Kind: "text", Content: w.canary + " reply"})
	must(err, "a chat reply")

	// A's own model-provider credentials (bring-your-own): a Claude account
	// whose NAME is the canary, and an OpenRouter API key. B has none; the
	// operator runs on the environment pool.
	w.creds, err = credentials.NewPG(pool, bytes.Repeat([]byte{3}, 32))
	must(err, "credential store")
	w.objA.ccAccount = w.canary
	must(w.creds.PutClaude(ua, w.canary, isoATok), "a claude account")
	must(w.creds.PutOpenRouter(ua, isoAORKey), "a openrouter key")

	w.up = newIsoUpstream(t)
	w.upHost = "iso.fake.test:" + w.up.port
	w.aConnName = "isoconn" + w.suffix
	_, err = cs.CreateConnector(ua, connectors.NewConnector{Name: w.aConnName, Hosts: []string{w.upHost},
		Scheme: connectors.SchemeBasic, Username: w.canary + "-user", Secret: w.canary + "-SECRET"})
	must(err, "a connector")
	// A's agent: an APPROVED grant (the positive control for the proxy), and a
	// PENDING request with a canary reason (the approve/deny/revoke target).
	_, err = pool.Exec(w.sys, `INSERT INTO connector_grants (agent_id, owner_id, connector_name, status, reason)
		VALUES ($1, $2, $3, 'approved', 'ok')`, w.a.agent.ID, w.a.uid, w.aConnName)
	must(err, "a approved grant")
	// A second connector whose NAME is the canary: the one no other user ever
	// names in a request, so its name on any of B's pages is A's data. (The
	// path connector's name is not a canary — B's walk sends it, and a grant B
	// files for a name is B's own row.)
	_, err = cs.CreateConnector(ua, connectors.NewConnector{Name: w.canary + "c", Hosts: []string{"api.example.test"},
		Scheme: connectors.SchemeBearer, Secret: "iso-list-secret-0123456789"})
	must(err, "a canary-named connector")
	must(cs.Audit(ua, connectors.AuditEntry{Kind: "proxy", AgentID: w.a.agent.ID, AgentName: w.a.agent.Name,
		Connector: w.aConnName, Method: "GET", Host: w.upHost, Path: "/", Status: 200, Detail: w.canary + " audit"}), "a audit")
	must(gh.Save(ua, w.canary+"-login", "repo", "gho_"+w.canary), "a github")

	// Personal tokens: one credential each for A and B, and A's second token
	// (canary-named) as the object. Minted through the real store as each
	// user, so row-level security decides the owner.
	ts := apitokens.NewPG(pool)
	w.apiTokens = map[int64]string{}
	for _, u := range []isoUser{w.a, w.b} {
		tok, _, err := ts.Create(as2(u), u.name+" cli", 0)
		must(err, u.name+" api token")
		w.apiTokens[u.uid] = tok
	}
	_, w.aToken2, err = ts.Create(ua, w.canary+" token", 0)
	must(err, "a second api token")
	w.aTokTask, err = ns.Create(ua, notes.Note{Title: w.canary + " token-control", Status: notes.StatusOpen})
	must(err, "a token-control task")
	w.aTokComment, err = ns.AddComment(ua, notes.Comment{NoteID: w.aTokTask.ID, Author: "a", Body: w.canary + " token comment"})
	must(err, "a token-control comment")

	// Browser flows: a runner token each (one host, so B's runner addresses
	// the queue A's jobs are in), A's two queued jobs with the canary in a
	// parameter, and A's second runner token.
	fs := flows.NewPG(pool)
	w.flowHost = "iso" + w.suffix
	w.runnerTokens = map[int64]string{}
	for _, u := range []isoUser{w.a, w.b} {
		tok, _, err := fs.CreateRunnerToken(as2(u), w.flowHost)
		must(err, u.name+" runner token")
		w.runnerTokens[u.uid] = tok
	}
	_, w.aRunner2, err = fs.CreateRunnerToken(ua, w.flowHost)
	must(err, "a second runner token")
	for _, j := range []*flows.Job{&w.aJob, &w.aJob2} {
		*j, err = fs.Enqueue(ua, flows.Request{Flow: "muse.poll", Host: w.flowHost, Instance: "isoinst",
			Params: map[string]string{"thread": w.canary + " thread"}}, "session", "iso")
		must(err, "a flow job")
	}

	// A's agent asks for a flow (the canary in its reason and params): the
	// object the approve/deny routes name.
	w.aFlowReq, err = fs.FileRequest(ua, w.a.agent.ID, w.a.uid, w.a.agent.Name, flows.Request{Flow: "muse.poll",
		Host: w.flowHost, Instance: "isoinst", Params: map[string]string{"thread": w.canary + " asked"}}, w.canary+" because")
	must(err, "a flow request")

	// Plugins: a token each, and A's canary-labelled second plugin.
	ps := plugins.NewPG(pool)
	w.pluginTokens = map[int64]string{}
	var aPlugin1 plugins.Plugin
	for _, u := range []isoUser{w.a, w.b} {
		tok, p, err := ps.Create(as2(u), "iso-inbox", "Iso inbox", "iso")
		must(err, u.name+" plugin")
		w.pluginTokens[u.uid] = tok
		if u.uid == w.a.uid {
			aPlugin1 = p
		}
	}
	_, w.aPlugin2, err = ps.Create(ua, "iso-second", w.canary+" plugin", "iso")
	must(err, "a second plugin")
	// A's agent was handed a message (the canary in its body) and drafted a
	// reply (the canary in it too): the object the draft routes name. Through
	// A's FIRST plugin, the one whose token the walk holds, so A's own result
	// call reaches its own draft (409: not claimed) where a ghost's is 404.
	aMsg, _, err := ps.Submit(ua, aPlugin1.ID, plugins.MessageIn{ExternalID: "iso-a-msg", Sender: "iso",
		Body: w.canary + " asks"})
	must(err, "a plugin message")
	_, err = ps.BeginDelivery(ua, aMsg.ID, w.a.agent.ID)
	must(err, "a plugin message being delivered")
	_, err = ps.RecordDelivery(ua, aMsg.ID, w.a.agent.ID, plugins.StatusDelivered)
	must(err, "a plugin message delivered")
	w.aDraft, _, err = ps.FileDraft(ua, w.a.agent.ID, w.a.uid, w.a.agent.Name, aMsg.ID, w.canary+" reply")
	must(err, "a reply draft")
	// A's chief routed that message to a mirrored session (0021): the object
	// POST /api/routes/{id}/reply names.
	aRoute, err := ps.CreateRoute(ua, w.a.agent.ID, w.a.uid, aMsg.ID,
		plugins.RouteTarget{Kind: "mirror", Key: "iso-session", Label: w.canary + " window"}, time.Hour)
	must(err, "a session route")

	// A's pins (0017): A's agent, and a window on A's mirrored host. B's pin
	// routes must not remove either (the snapshot holds them).
	pinStore := pins.NewPG(pool)
	must(pinStore.PinAgent(ua, w.a.agent.ID), "a agent pin")
	must(pinStore.PinMirror(ua, pins.MirrorKey{Host: w.canary + "-host", Session: "main", Window: "@1"}), "a mirror pin")
	// A's recently viewed entries (0018): B's page visits record B's own rows,
	// never A's (the snapshot holds them).
	recentStore := recents.NewPG(pool)
	must(recentStore.VisitAgent(ua, w.a.agent.ID), "a agent visit")
	must(recentStore.VisitMirror(ua, pins.MirrorKey{Host: w.canary + "-host", Session: "main", Window: "@1"}), "a mirror visit")
	// A's session focus (0020): B's tools and pages read and write B's own.
	_, ferr := focus.NewPG(pool).Upsert(ua, focus.AgentSubject(w.a.agent.ID), focus.Write{Title: w.canary + " focus", Source: focus.SourceSummarizer})
	must(ferr, "a focus")

	// A's queued message (0019), for a window on A's mirrored host, its body
	// carrying the canary.
	// A's file attachment (0024): its bytes and its filename carry the canary,
	// so A's own view of it is told apart from the ghost's by the body.
	w.uploads = &uploadstest.Objects{}
	upSigner, err := uploads.NewSigner("iso-walk-upload-signing-key-0123456789abcdef")
	must(err, "upload signer")
	w.uploadSvc = &uploads.Service{Store: uploads.NewPG(pool), Objects: w.uploads, Signer: upSigner,
		PublicURL: "https://muster.test", MaxBytes: 1 << 20, LinkTTL: time.Hour}
	aUpload, err := w.uploadSvc.Put(ua, strings.NewReader(w.canary+" attached notes\n"), w.canary+"-notes.txt")
	must(err, "a upload")

	aQueued, err := sendqueue.NewPG(pool).Enqueue(ua, sendqueue.Item{
		Target: sendqueue.Target{Host: w.canary + "-host", Session: "main", Window: "@1"}, Body: w.canary + " queued", StepUpAt: time.Now()})
	must(err, "a queued message")
	// A's open ask (0025), asked by A's agent, its question carrying the canary.
	aAsk, err := attention.NewPG(pool).Create(ua, attention.New{Asker: attention.Asker{AgentID: w.a.agent.ID, Label: w.a.agent.Name},
		Question: w.canary + " asks", Options: []string{"yes", "no"}})
	must(err, "an ask")

	w.aIdentities = []string{w.a.agent.Name, agents.ChiefNameFor(w.a.uid), w.aSessionLink, w.aTag, w.aProject}
	w.opInvite, err = users.NewPG(pool).CreateInvite(ctx, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession},
		bytes.Repeat([]byte{byte(time.Now().UnixNano())}, 32), "", time.Now().Add(time.Hour))
	must(err, "op invite")
	w.objA = isoObjs{ccAccount: w.objA.ccAccount, user: w.a.uid, invite: w.opInvite.ID, task: w.a.task.ID, agent: w.a.agent.ID, grant: w.aGrantID, comment: w.aComment.ID,
		attach: w.aAttachment.ID, chat: w.aChatSession.ID, agentName: w.a.agent.Name, connName: w.aConnName,
		tag: w.aTag, link: w.aSessionLink, mirrorHost: w.canary + "-host", mirrorSession: "main", token: w.aToken2.ID,
		pushDevice: w.objA.pushDevice, flowJob: w.aJob.ID, runnerTok: w.aRunner2.ID, flowReq: w.aFlowReq.ID,
		plugin: w.aPlugin2.ID, draft: w.aDraft.ID, queued: aQueued.ID, route: aRoute.RouteID, upload: aUpload.PublicID, ask: aAsk.ID,
		pluginMsg: aMsg.ID}
	// The ghost: ids and names that exist for NOBODY. A refusal of A's object
	// must be indistinguishable from the answer about the ghost.
	const ghostID = 9_000_000_000
	w.ghost = isoObjs{user: ghostID, invite: ghostID, task: ghostID, agent: ghostID, grant: ghostID, comment: ghostID, attach: ghostID, chat: ghostID,
		agentName: "ghost-agent" + w.suffix, connName: "ghostconn" + w.suffix, tag: "ghosttag", link: "ghost-sess-" + w.suffix,
		mirrorHost: "ghost-host", mirrorSession: "main", ccAccount: "ghostacct", token: ghostID, pushDevice: ghostID,
		flowJob: ghostID, runnerTok: ghostID, flowReq: ghostID, plugin: ghostID, draft: ghostID, passkey: "00000000-0000-4000-a000-00000000ghost",
		queued: ghostID, route: "rt_000000000000000000000000", upload: "GhostGhostGhostGhost00", ask: ghostID, pluginMsg: ghostID,
		device: ghostID}

	// Paired devices (0039): each user pairs one, through the real store and a
	// real pairing code; A's is canary-named (Account → Devices shows it).
	ds := devices.NewPG(pool)
	w.deviceTokens = map[int64]string{}
	for _, u := range []isoUser{w.a, w.b} {
		code, _, err := ds.CreatePairingCode(as2(u), time.Now())
		must(err, u.name+" pairing code")
		name := "iso phone"
		if u.uid == w.a.uid {
			name = w.canary + " phone"
		}
		tok, d, err := ds.Redeem(context.Background(), code, devices.PairIn{Name: name, AppVersion: "1.0"})
		must(err, u.name+" device")
		w.deviceTokens[u.uid] = tok
		if u.uid == w.a.uid {
			w.aDevice = d
		}
	}
	w.objA.device = w.aDevice.ID

	// A's push device: through the real store, as the claim the subscribe route
	// makes. Its user agent and endpoint carry the canary.
	pushStore := push.NewPG(pool)
	w.aEndpoint = "https://fcm.googleapis.com/fcm/send/" + w.canary
	var aSub push.Subscription
	aSub.Endpoint, aSub.Keys.P256dh, aSub.Keys.Auth = w.aEndpoint, "p256-"+w.canary, "auth-"+w.canary
	_, err = pushStore.Claim(ctx, w.a.uid, aSub, w.canary+" browser")
	must(err, "a push device")
	aDevs, err := pushStore.Devices(ua)
	must(err, "a push devices")
	w.objA.pushDevice = aDevs[0].ID
	w.pusher = &isoPusher{}

	// The server, fully wired over the real stores.
	w.bus = sse.New(64)
	srv := New(w.bus, AuthConfig{HookToken: isoHookToken, ServiceToken: isoServiceToken, UIPassword: isoPassword},
		log.New(io.Discard, "", 0))
	w.prov = &isoProvisioner{store: as}
	envPool, err := ccpool.NewStatic(map[string]string{isoEnvAccount: isoEnvToken})
	must(err, "env pool")
	plane := &credentials.Plane{EnvClaude: envPool, EnvOpenRouterKey: "sk-or-v1-iso-ENV-operator-key-000000",
		Personal: w.creds, Roles: users.NewPG(pool)}
	ccp, err := ccpool.New(plane, ccpool.NewPG(pool))
	must(err, "account pool")
	ext := Extensions{
		Kinds: isoKinds{pool: ccp, plane: plane}, Credentials: isoCredPage{PGStore: w.creds, plane: plane},
		Notes: ns, Agents: as, GitHub: gh, Users: users.NewPG(pool), APITokens: apitokens.NewPG(pool),
		GitHubOAuth: GitHubOAuthConfig{ClientID: "iso-client", ClientSecret: "iso-secret", BaseURL: "http://muster.test",
			StateKey: []byte("iso-walk-oauth-state-key-0123456789")},
		Provisioner: w.prov, Gateway: w.prov, Connectors: cs,
		Mirror: &isoMirror{owner: w.a.uid, host: w.canary + "-host"},
		Flows:  flows.NewPG(pool),
		// Plugins ON, with no gateway: a message B's plugin posts is stored as
		// B's and stays queued.
		Plugins: plugins.NewPG(pool),
		Pins:    pins.NewPG(pool),
		Recents: recents.NewPG(pool),
		// Session routing (0021), on: A's route is a real object B must not reach.
		Routes:    plugins.NewPG(pool),
		Routing:   &Routing{},
		Focus:     focus.NewPG(pool),
		SendQueue: sendqueue.NewPG(pool),
		Uploads:   w.uploadSvc,
		// The attention queue (0025): A's ask is a real object B must not reach.
		Attention: attention.NewPG(pool),
		Devices:   devices.NewPG(pool),
		Push:      push.NewService(pushStore, w.pusher, push.VAPIDConfig{PublicKey: "iso-walk-vapid-public"}, log.New(io.Discard, "", 0)),
		ConnectorPolicy: connectors.Policy{AllowLoopback: true, InsecureHTTP: true,
			Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
				if strings.HasSuffix(host, ".fake.test") {
					return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
				}
				return nil, fmt.Errorf("no such host %s", host)
			}},
	}
	srv.UseExtensions(ext)
	w.router = &isoRouter{dirs: []string{"/srv/projects/" + w.opCanary}}
	srv.UseRouter(w.router)
	// The admin pages need GoTrue and the account store; the session tier stays
	// password mode (sessionUserOverride below).
	w.gt = gotruetest.New(t, bytes.Repeat([]byte{5}, 40))
	acct := users.NewPG(pool)
	srv.UseGoTrue(GoTrueAuth{Client: w.gt.Client(), Verifier: w.gt.Verifier(), Users: acct, Accounts: acct})
	srv.auth.PublicURL = "https://muster.test"
	srv.UseApprovalGate(&isoGate{})

	// The gotrue-mode twin: same stores, same router and gate, its sessions
	// resolved by the real session tier (requireGoTrueSession → users.gotrue_sub
	// → users row) from cookies the fake issuer signed. A and B are bound to
	// GoTrue subjects the way a claim or an invite binds them.
	w.gtFake = gotruetest.New(t, []byte("iso-walk-gotrue-secret-0123456789abcdef"))
	gtSrv := New(w.bus, AuthConfig{Mode: AuthModeGoTrue, HookToken: isoHookToken, ServiceToken: isoServiceToken},
		log.New(io.Discard, "", 0))
	gtSrv.UseExtensions(ext)
	gtSrv.UseRouter(w.router)
	gtSrv.UseApprovalGate(&isoGate{})
	// Accounts too, so the twin SERVES the admin pages (accounts PR 5): a
	// member's real gotrue session must be refused them by its role, not by a
	// 503 for a missing store (TestAMemberCannotReachAnyAdminRoute).
	gtStore := users.NewPG(pool)
	gtSrv.UseGoTrue(GoTrueAuth{Client: w.gtFake.Client(), Verifier: w.gtFake.Verifier(), Users: gtStore, Accounts: gtStore})
	w.gtH = BrowserListener(gtSrv.Handler())
	w.gtTokens = map[int64][2]string{}
	for _, u := range []isoUser{w.a, w.b} {
		sub := w.gtFake.AddUser(u.name+w.suffix+"@example.com", "iso-walk-password-0123")
		if _, err := pool.Exec(w.sys, `UPDATE users SET gotrue_sub = $1 WHERE id = $2`, sub, u.uid); err != nil {
			t.Fatal(err)
		}
		at, rt, _ := w.gtFake.Mint(sub)
		w.gtTokens[u.uid] = [2]string{at, rt}
		if u.uid == w.a.uid {
			w.aSub = sub
		} else {
			w.bSub = sub
		}
	}
	// Passkeys on in the twin's GoTrue, and A holds one: the target of B's
	// POST /account/passkeys/{id}/delete. GoTrue — not muster's database —
	// owns passkeys, so sameRows cannot see them; the walk's end checks B
	// cannot remove A's (TestNoRouteLetsOneUserReachAnothersData).
	w.gtFake.EnablePasskeys()
	w.objA.passkey = w.gtFake.AddPasskey(w.aSub, "iso-a-credential-"+w.suffix)
	srv.sessionUserOverride = func(r *http.Request) int64 {
		if v, err := strconv.ParseInt(r.Header.Get(isoUserHeader), 10, 64); err == nil && v > 0 {
			return v
		}
		return authz.OperatorUserID
	}
	w.srv = srv
	w.h = srv.Handler()
	w.connectH = OnlyConnectSurface(w.h)

	// A's chief, through the REAL provision path (POST /chief/provision as A's
	// session) — not a hand-set display name, so the walk sees the chief a
	// member actually gets.
	if rec := w.do(w.h, "POST", "/chief/provision", url.Values{"model": {""}}.Encode(), cred{sessionUser: w.a.uid}); rec.Code != http.StatusOK {
		t.Fatalf("seed: A's chief provision: %d %s", rec.Code, rec.Body)
	}
	w.aChief, err = as.GetByName(ua, agents.ChiefNameFor(w.a.uid))
	must(err, "a chief")
	must(pool.QueryRow(w.sys, `INSERT INTO connector_grants (agent_id, owner_id, connector_name, status, reason)
		VALUES ($1, $2, $3, 'pending', $4) RETURNING id`, w.aChief.ID, w.a.uid, w.aConnName, w.canary+" reason").Scan(&w.aGrantID), "a pending grant")
	w.objA.grant = w.aGrantID
	// An ACTIVE TURN of A's chief (round-2 audit 🟡1): the notifications
	// panel's ACTIVE section reads a SERVER-WIDE in-memory registry, not a
	// table, so row-level security never sees it. The chief has no unread
	// messages, so its name on a panel can only come from this registry.
	// (markAgentActive is exactly what handleAgentWS calls at turn start.)
	srv.markAgentActive(w.a.uid, w.aChief.Name, "thinking")
	// The operator's own tag and project: the positive control for the admin's
	// vocabulary routes, whose leak check looks for A's.
	_, err = ns.AddTags(authz.Operator(ctx), w.op.task.ID, []string{"isoop" + w.suffix, "project:isoopproj" + w.suffix})
	must(err, "op tags")

	// The operator's agent files one approval: the only kind of id there is,
	// and the target for B's GET/DELETE /agent/approvals/{id}.
	rec := w.do(w.h, "POST", "/agent/approvals", `{"tool":"Bash","command":"ls"}`, cred{agentToken: w.op.token})
	var filed struct {
		RequestID string `json:"requestId"`
	}
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &filed) != nil || filed.RequestID == "" {
		t.Fatalf("control: the operator's agent could not file an approval: %d %s", rec.Code, rec.Body)
	}
	w.opApproval = filed.RequestID

	// A signs in through the gotrue twin BEFORE anyone else, so anything in the
	// session tier that keys a user by the wrong thing (a cache, a lookup) would
	// hand A's identity to the next caller — which the B walk below then is.
	if rec := w.do(w.h, "GET", "/tasks", "", cred{gotrueUser: w.a.uid}); rec.Code != http.StatusOK {
		t.Fatalf("seed: A's gotrue session: %d %s", rec.Code, truncate(rec.Body.String(), 200))
	}

	t.Cleanup(func() {
		bg := authz.System(context.Background())
		ids := []int64{w.a.uid, w.b.uid}
		for _, q := range []string{
			`DELETE FROM connector_audit WHERE owner_id = ANY($1)`, `DELETE FROM connector_grants WHERE owner_id = ANY($1)`,
			`DELETE FROM connectors WHERE owner_id = ANY($1)`, `DELETE FROM chat_messages WHERE owner_id = ANY($1)`,
			`DELETE FROM chat_sessions WHERE owner_id = ANY($1)`, `DELETE FROM agents WHERE owner_id = ANY($1)`,
			`DELETE FROM task_sessions WHERE owner_id = ANY($1)`, `DELETE FROM note_attachments WHERE owner_id = ANY($1)`,
			`DELETE FROM note_comments WHERE owner_id = ANY($1)`, `DELETE FROM notes WHERE owner_id = ANY($1)`,
			`DELETE FROM github_connection WHERE owner_id = ANY($1)`, `DELETE FROM user_credentials WHERE owner_id = ANY($1)`,
			`DELETE FROM cc_account_marks WHERE owner_id = ANY($1)`,
			`DELETE FROM push_subscriptions WHERE owner_id = ANY($1)`, `DELETE FROM push_preferences WHERE owner_id = ANY($1)`,
			`DELETE FROM device_pairing_codes WHERE owner_id = ANY($1)`, `DELETE FROM devices WHERE owner_id = ANY($1)`,
			`DELETE FROM api_tokens WHERE user_id = ANY($1)`, `DELETE FROM audit_log WHERE user_id = ANY($1)`,
			`DELETE FROM flow_requests WHERE owner_id = ANY($1)`, `DELETE FROM flow_jobs WHERE owner_id = ANY($1)`, `DELETE FROM flow_runner_tokens WHERE owner_id = ANY($1)`,
			`DELETE FROM plugins WHERE owner_id = ANY($1)`,
			`DELETE FROM users WHERE id = ANY($1)`,
		} {
			_, _ = pool.Exec(bg, q, ids)
		}
		_, _ = pool.Exec(bg, `DELETE FROM invites WHERE id=$1`, w.opInvite.ID)
		_, _ = pool.Exec(bg, `DELETE FROM agents WHERE id=$1`, w.op.agent.ID)
		_, _ = pool.Exec(bg, `DELETE FROM notes WHERE id=$1`, w.op.task.ID)
	})
	return w
}

// cred is one way of authenticating a request.
type cred struct {
	sessionUser int64  // a browser session acting as this user
	gotrueUser  int64  // a REAL gotrue session (cookies the fake issuer signed) of this user
	agentToken  string // an agent's own token
	hook        bool   // the shared machine hook token (user 1)
	apiToken    string // a user's own personal API token (mst_…)
	runnerToken string // a user's own browser-flow runner token (mfr_…)
	pluginToken string // a user's own plugin token (mpl_…)
	deviceToken string // a user's own paired device credential (mdv_…)
}

func (c cred) String() string {
	switch {
	case c.sessionUser > 0:
		return fmt.Sprintf("session(user %d)", c.sessionUser)
	case c.gotrueUser > 0:
		return fmt.Sprintf("gotrue-session(user %d)", c.gotrueUser)
	case c.agentToken != "":
		return "agent-token"
	case c.hook:
		return "hook"
	case c.apiToken != "":
		return "api-token"
	case c.runnerToken != "":
		return "runner-token"
	case c.pluginToken != "":
		return "plugin-token"
	case c.deviceToken != "":
		return "device-token"
	}
	return "none"
}

func (w *isoWorld) do(h http.Handler, method, path, body string, c cred) *httptest.ResponseRecorder {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctype := ""
	switch {
	case strings.HasPrefix(body, "multipart:"):
		vals, _ := url.ParseQuery(strings.TrimPrefix(body, "multipart:"))
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for k, vs := range vals {
			for _, v := range vs {
				_ = mw.WriteField(k, v)
			}
		}
		_ = mw.Close()
		body, ctype = buf.String(), mw.FormDataContentType()
	case strings.HasPrefix(body, "{"):
		ctype = "application/json"
	case body != "":
		ctype = "application/x-www-form-urlencoded"
	}
	if c.gotrueUser > 0 {
		h = w.gtH // the gotrue twin, on its browser listener
	}
	r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	r.Host = "muster.test"
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	switch {
	case c.sessionUser > 0:
		r.Header.Set("Cookie", sessionCookieFor(w.srv))
		r.Header.Set("Origin", "http://muster.test")
		r.Header.Set(isoUserHeader, strconv.FormatInt(c.sessionUser, 10))
		r.Header.Set("HX-Request", "true")
	case c.gotrueUser > 0:
		tk := w.gtTokens[c.gotrueUser]
		r.Header.Set("Cookie", accessCookieName+"="+tk[0]+"; "+refreshCookieName+"="+tk[1])
		r.Header.Set("Origin", "http://muster.test")
		r.Header.Set("HX-Request", "true")
	case c.agentToken != "":
		r.Header.Set("X-Muster-Token", c.agentToken)
	case c.hook:
		r.Header.Set("X-Muster-Token", isoHookToken)
	case c.apiToken != "":
		r.Header.Set("Authorization", "Bearer "+c.apiToken)
	case c.runnerToken != "":
		r.Header.Set("Authorization", "Bearer "+c.runnerToken)
	case c.pluginToken != "":
		r.Header.Set("Authorization", "Bearer "+c.pluginToken)
	case c.deviceToken != "":
		r.Header.Set("Authorization", "Bearer "+c.deviceToken)
	}
	rec := httptest.NewRecorder()
	func() {
		defer func() { _ = recover() }() // a handler panic is a 500, as recoverMiddleware makes it
		h.ServeHTTP(rec, r)
	}()
	return rec
}

// isoObjs names one set of objects a request can point at.
type isoObjs struct {
	ccAccount                                 string
	user, invite                              int64
	task, agent, grant, comment, attach, chat int64
	token                                     int64  // a personal API token's id
	flowJob, runnerTok, flowReq               int64  // a browser-flow job's, runner token's and agent request's id
	plugin                                    int64  // a plugin's id (0015)
	draft                                     int64  // a plugin reply draft's id (0015/0016)
	passkey                                   string // a GoTrue passkey id (the gotrue twin's fake)
	pushDevice                                int64  // a Web Push device's id
	device                                    int64  // a paired device's id (0039)
	agentName, connName, tag, link            string
	mirrorHost, mirrorSession                 string
	queued                                    int64  // a queued message's id (0019)
	upload                                    string // a file attachment's public id (0024)
	route                                     string // a session route's id (0021)
	pluginMsg                                 int64  // a plugin message's id (its retry route)
	ask                                       int64  // an attention ask's id (0025)
}

// fill turns a route pattern into a concrete request naming objects o, with a
// body that would succeed for o's owner (actor's own task is the merge winner).
func (w *isoWorld) fill(pattern string, o isoObjs, actor isoUser) (method, path, body string) {
	method, path = http.MethodGet, pattern
	if i := strings.IndexByte(pattern, ' '); i > 0 {
		method, path = pattern[:i], pattern[i+1:]
	}
	id := strconv.FormatInt(o.task, 10)
	switch {
	case strings.Contains(path, "/agents/{id}") || strings.HasPrefix(path, "/ui/agents/{id}"):
		id = strconv.FormatInt(o.agent, 10)
	case strings.Contains(path, "connector-grants/{id}") || strings.Contains(path, "connector-requests/{id}"):
		id = strconv.FormatInt(o.grant, 10)
	case strings.Contains(path, "/agent/approvals/{id}"):
		id = w.opApproval
		if o.task == w.ghost.task {
			id = "ghost-card"
		}
	case strings.Contains(path, "/api/sessions/{id}"):
		id = o.link
	case strings.Contains(path, "/runner-tokens/{id}"):
		// Before "tokens/{id}" below, which would otherwise match it.
		id = strconv.FormatInt(o.runnerTok, 10)
	case strings.HasPrefix(path, "/plugins/{id}"):
		id = strconv.FormatInt(o.plugin, 10)
	case strings.HasPrefix(path, "/plugins/drafts/{id}") || strings.HasPrefix(path, "/plugin/v1/drafts/{id}"):
		id = strconv.FormatInt(o.draft, 10)
	case strings.Contains(path, "/flows/requests/{id}"):
		id = strconv.FormatInt(o.flowReq, 10)
	case strings.Contains(path, "flows/jobs/{id}") || strings.HasPrefix(path, "/runner/flows/{id}"):
		id = strconv.FormatInt(o.flowJob, 10)
	case strings.Contains(path, "tokens/{id}"):
		id = strconv.FormatInt(o.token, 10)
	case strings.HasPrefix(path, "/account/passkeys/{id}"):
		id = o.passkey
	case strings.Contains(path, "/account/push/{id}"):
		id = strconv.FormatInt(o.pushDevice, 10)
	case strings.HasPrefix(path, "/account/devices/{id}"):
		id = strconv.FormatInt(o.device, 10)
	case strings.HasPrefix(path, "/admin/users/{id}"):
		id = strconv.FormatInt(o.user, 10)
	case strings.HasPrefix(path, "/admin/invites/{id}"):
		id = strconv.FormatInt(o.invite, 10)
	case strings.HasPrefix(path, "/send-queue/{id}"):
		id = strconv.FormatInt(o.queued, 10)
	case strings.HasPrefix(path, "/api/routes/{id}"):
		id = o.route
	case strings.HasPrefix(path, "/plugins/messages/{id}") || strings.HasPrefix(path, "/api/plugins/messages/{id}"):
		id = strconv.FormatInt(o.pluginMsg, 10)
	case strings.HasPrefix(path, "/uploads/{id}") || strings.HasPrefix(path, "/f/{id}"):
		id = o.upload
	case strings.HasPrefix(path, "/attention/asks/{id}"):
		id = strconv.FormatInt(o.ask, 10)
	}
	name := o.agentName
	if strings.Contains(path, "connectors/{name}") {
		name = o.connName
	}
	if strings.Contains(path, "credentials/claude/{name}") {
		name = o.ccAccount
	}
	path = strings.NewReplacer(
		"{$}", "", "{id}", url.PathEscape(id), "{name}", url.PathEscape(name),
		"{cid}", strconv.FormatInt(o.comment, 10), "{aid}", strconv.FormatInt(o.attach, 10),
		"{tag}", url.PathEscape(o.tag), "{sid}", strconv.FormatInt(o.chat, 10),
		"{connector}", o.connName, "{host}", w.upHost, "{path...}", "probe",
	).Replace(path)
	if path == "/static/" {
		path = "/static/app.css"
	}
	if strings.HasPrefix(path, "/mirror/terminal") || strings.HasPrefix(path, "/mirror/view") || path == "/mirror/confirm-send" ||
		path == "/send-queue" || path == "/send-queue/confirm" || path == "/attention/hermes/approve" {
		if method == http.MethodPost {
			// `kind` and `password` are the plain confirm page's own fields.
			return method, path, url.Values{"host": {o.mirrorHost}, "session": {o.mirrorSession}, "text": {"iso-write"},
				"kind": {"terminal"}, "password": {"iso-not-the-password"}, "lines": {"3"}}.Encode()
		}
		path += "?" + url.Values{"host": {o.mirrorHost}, "session": {o.mirrorSession}}.Encode()
	}
	if path == "/ui/focus/history" {
		// Focus history (0022) names its session by a query parameter: A's agent.
		path += "?" + url.Values{"subject": {"agent:" + strconv.FormatInt(o.agent, 10)}}.Encode()
	}
	if method == http.MethodGet || method == http.MethodDelete {
		if strings.HasPrefix(path, "/github/callback") {
			path += "?state=x&code=y"
		}
		return method, path, ""
	}
	if strings.HasPrefix(path, "/plugin/") {
		// A whole valid message: what B's plugin posts is accepted as B's own.
		// The draft routes' fields ride along: a claim that does not wait, and a
		// result (so a refusal is about WHOSE draft, not a malformed body).
		return method, path, `{"external_id":"iso-` + w.suffix + `-` + strconv.FormatInt(actor.uid, 10) +
			`","sender":"iso","subject":"iso-write","body":"iso-write","wait_seconds":0,"ok":true}`
	}
	if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/agent/") || path == "/mcp" || strings.HasPrefix(path, "/connect/") ||
		strings.HasPrefix(path, "/runner/") {
		// The runner door's fields ride along: a claim naming the instance A's
		// jobs are queued on (a one-second wait), and a result's outcome.
		return method, path, `{"jsonrpc":"2.0","id":1,"method":"tools/list","title":"iso-write","body":"iso-write",` +
			`"status":"in_progress","message":"iso-write","answer":"iso-write","reason":"iso-write","blocked":true,"tool":"Bash","command":"ls",` +
			`"instances":["isoinst"],"waitSeconds":1,"outcome":"done"}`
	}
	winner := actor.task.ID
	if actor.uid == w.a.uid {
		winner = w.aTask2.ID
	}
	f := url.Values{
		"title": {"iso-write"}, "body": {"iso-write"}, "status": {"in_progress"}, "tags": {"isowrite"},
		"name": {"iso-name"}, "model": {""}, "secret": {"iso-rotated-secret-0123456789"},
		"note_text": {"iso"}, "directory": {"/tmp"}, "answer": {"iso-write"},
		// Cross-user links attempted through FORM fields, not the path: the
		// actor's own task wins a merge whose loser is o's task, and a dispatch
		// names o's task.
		"winner": {strconv.FormatInt(winner, 10)}, "loser": {strconv.FormatInt(o.task, 10)},
		"note_id": {strconv.FormatInt(o.task, 10)},
	}
	// The pin routes name o's agent. Only on them: `kind` is also the dispatch
	// form's agent-kind field.
	if strings.HasPrefix(pattern, "POST /agents/pins") {
		f.Set("kind", "agent")
		f.Set("agent_id", strconv.FormatInt(o.agent, 10))
	}
	if pattern == "POST /tasks" {
		return method, path, "multipart:" + f.Encode()
	}
	return method, path, f.Encode()
}

// leaks reports which of A's strings body carries.
func (w *isoWorld) leaks(body string, withIdentities bool) []string {
	var out []string
	if strings.Contains(body, w.canary) {
		out = append(out, "canary")
	}
	if withIdentities {
		for _, id := range w.aIdentities {
			if strings.Contains(body, id) {
				out = append(out, id)
			}
		}
	}
	return out
}

// snapshot is every row A owns, as JSON, from System's view.
func (w *isoWorld) snapshot() map[string]string {
	w.t.Helper()
	out := map[string]string{}
	for _, tbl := range []string{"notes", "note_comments", "note_attachments", "task_sessions", "agents",
		"chat_sessions", "chat_messages", "connectors", "connector_grants", "connector_audit", "github_connection", "mirror_tokens",
		"user_credentials", "cc_account_marks", "flow_jobs", "flow_runner_tokens", "push_subscriptions", "push_preferences", "flow_requests",
		"plugins", "plugin_messages", "plugin_reply_drafts", "pins", "recents", "send_queue", "agent_focus", "agent_focus_history", "session_routes", "uploads", "attention_asks", "jobs",
		"devices", "device_pairing_codes"} {
		var js string
		if err := w.pool.QueryRow(w.sys, `SELECT coalesce(json_agg(t ORDER BY t.id)::text, '[]') FROM `+tbl+` t WHERE owner_id=$1`, w.a.uid).Scan(&js); err != nil {
			// task_sessions/github_connection have no `id` ordering column in every
			// shape; fall back to a stable text ordering.
			if err := w.pool.QueryRow(w.sys, `SELECT coalesce(json_agg(t ORDER BY t::text)::text, '[]') FROM `+tbl+` t WHERE owner_id=$1`, w.a.uid).Scan(&js); err != nil {
				w.t.Fatalf("snapshot %s: %v", tbl, err)
			}
		}
		out[tbl] = js
	}
	// The account tables are keyed by user_id, not owner_id (accounts PR 6:
	// B's revoke of A's token, or an audit row written as A, would show here).
	for _, tbl := range []string{"api_tokens", "audit_log"} {
		var js string
		if err := w.pool.QueryRow(w.sys, `SELECT coalesce(json_agg(t ORDER BY t.id)::text, '[]') FROM `+tbl+` t WHERE user_id=$1`, w.a.uid).Scan(&js); err != nil {
			w.t.Fatalf("snapshot %s: %v", tbl, err)
		}
		out[tbl] = js
	}
	// The account rows an admin-only route could change: A's user row and the
	// operator's pending invite (accounts PR 5).
	for name, q := range map[string]string{
		"users":   `SELECT row_to_json(u)::text FROM users u WHERE id=$1`,
		"invites": `SELECT row_to_json(i)::text FROM invites i WHERE id=$1`,
	} {
		id := w.a.uid
		if name == "invites" {
			id = w.opInvite.ID
		}
		var js string
		if err := w.pool.QueryRow(w.sys, q, id).Scan(&js); err != nil {
			w.t.Fatalf("snapshot %s: %v", name, err)
		}
		out[name] = js
	}
	return out
}

// --- the walk ------------------------------------------------------------------

type isoResult struct {
	route, via      string
	status, ghost   int // the answer about the real object, and about the ghost
	body, ghostBody string
}

// walkAs drives every route as actor through each credential (an agent token
// on BOTH the main handler and the connect port), naming objects o — and
// again naming the ghost, which exists for nobody.
func (w *isoWorld) walkAs(actor isoUser, o isoObjs, creds []cred, routes []string) []isoResult {
	var out []isoResult
	for _, route := range routes {
		for _, c := range creds {
			hs := map[string]http.Handler{"main": w.h}
			if c.agentToken != "" {
				hs["connect-port"] = w.connectH
			}
			for _, via := range []string{"main", "connect-port"} {
				h, ok := hs[via]
				if !ok {
					continue
				}
				res := isoResult{route: route, via: c.String() + "@" + via}
				if namesObject(route) {
					m, p, b := w.fill(route, w.ghost, actor)
					g := w.do(h, m, p, b, c)
					res.ghost, res.ghostBody = g.Code, g.Body.String()
				}
				m, p, b := w.fill(route, o, actor)
				rec := w.do(h, m, p, b, c)
				res.status, res.body = rec.Code, rec.Body.String()
				out = append(out, res)
			}
		}
	}
	return out
}

// namesObject reports whether a route names an object — in its path, or (for
// the dispatch and merge forms) in its body.
func namesObject(route string) bool {
	return paramRE.MatchString(route) || route == "POST /agents" || route == "POST /tasks/merge" ||
		strings.HasPrefix(route, "GET /mirror/terminal") || strings.HasPrefix(route, "GET /mirror/view") ||
		route == "POST /mirror/view/send" || route == "POST /mirror/terminal/send" || route == "POST /mirror/confirm-send" ||
		route == "POST /mirror/terminal/scroll" || route == "POST /mirror/view/ask" || route == "POST /attention/hermes/approve" ||
		route == "GET /send-queue" || route == "POST /send-queue" || route == "POST /send-queue/confirm" ||
		// Focus history (0022) names its session in the query (?subject=agent:<id>),
		// so it is held to the object routes' rules: B's answer about A's agent
		// must be its answer about a ghost, and A's own walk must reach it. (That
		// B gets a 404 rather than an empty 200 is pinned by
		// TestFocusHistoryRouteIs404ForAnotherOwnersSession, not here: both
		// answers are indistinguishable either way.)
		route == "GET /ui/focus/history"
}

var paramRE = regexp.MustCompile(`\{[a-z]`)

func sortedRoutes() []string {
	var out []string
	for r := range isolationClasses {
		out = append(out, r)
	}
	// Destructive routes last, children before parents, so the control walk as
	// A can run them all against one fixture.
	rank := func(r string) int {
		switch {
		case strings.HasSuffix(r, "/delete"): // a POST that deletes (a plain HTML form cannot DELETE)
			return 2
		case !strings.HasPrefix(r, "DELETE "):
			return 0
		case strings.Contains(r, "{cid}"), strings.Contains(r, "{tag}"), strings.Contains(r, "grants"), strings.Contains(r, "approvals"):
			return 1
		default:
			return 2
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := rank(out[i]), rank(out[j]); ri != rj {
			return ri < rj
		}
		return out[i] < out[j]
	})
	return out
}

// sideEffectControlled are the routes whose answer to their OWNER is the same
// as their answer about a ghost (fire-and-forget or acknowledge-only), so the
// control cannot be read off the status. Each has its control asserted on the
// side effect instead, at the end of the walk.
var sideEffectControlled = map[string]string{
	"POST /agents/{id}/start": "the provisioner acted for A (isoProvisioner records it), never for B",
	// 🔴 A BEARER CAPABILITY, NOT A SIDE EFFECT: the signed link's credential is
	// the HMAC in its query, which no walker holds — the walk sends the bare
	// path, so A's own answer is the ghost's 404, as it must be. Its control is
	// TestTheSignedLinkServesTheFileAndNothingElse (uploads_test.go): the SAME
	// route with A's valid signature serves A's bytes, and unsigned, tampered,
	// expired or revoked it is that 404.
	"GET /f/{id}": "a signed link (uploads.go): its credential is the HMAC no walker holds; controlled in uploads_test.go",
	// 🔴 A DOOR NO WALKER HOLDS, AS GET /f/{id}: the job API is the SERVICE tier
	// only (requireServiceToken), which acts as user 1, so no credential of a
	// member — A's included — opens it, and A's answer is the ghost's 401. Its
	// owner isolation is pinned at the store, where it is enforced:
	// TestThePGStoreDedupsAnIdempotencyKeyPerOwner (internal/jobs) reads another
	// owner's job as ErrNotFound and refuses a job on another owner's agent.
	"GET /api/jobs/{id}":                    "the service tier only (user 1); per-owner isolation controlled in internal/jobs's pg tests",
	"POST /agents/{id}/stop":                "as start",
	"DELETE /agents/{id}":                   "as start (Destroy)",
	"POST /agents/sessions/{sid}/read":      "A's chat_sessions.read_at moved; the mutation leg shows B's did not",
	"POST /agent/approvals":                 "B's agent is refused 503 (a member); the admin agent files (newIsoWorld's control)",
	"/connect/{connector}/{host}/{path...}": "A's secret reached the upstream for A, never for B",
	// Only the gotrue twin serves passkeys, and A's walk ends A's gotrue
	// session at POST /logout before the {id} routes sort — so the control is
	// run at the end of the walk on a FRESH session of A's: it removes A's
	// passkey (303, gone from GoTrue), after a fresh session of B's was
	// refused it (404, still there).
	"POST /account/passkeys/{id}/delete": "fresh sessions: B's delete of A's passkey is refused and leaves it; A's removes it",
}

// controlMarkers: a route whose owner and ghost answers share a status, where
// a string in the OWNER's body is what tells them apart.
var controlMarkers = map[string]string{
	"GET /agent/connectors/{name}":          `"exists":true`,
	"POST /agent/connectors/{name}/request": `"exists":true`,
}

// echoOnly: routes that render the same thing for ANY name — their own and a
// ghost alike — echoing only the name the caller sent, so there is nothing an
// owner sees that an outsider does not. The leak check still runs on them.
var echoOnly = map[string]string{
	"GET /ui/connectors/{name}/rotate": "renders the rotate form for whatever name is in the URL; the rotate itself (POST) is owner-scoped",
}

// checkOutsider asserts what an outsider's (B's, or the admin's against a
// member) walk may show: no canary anywhere, no identifier of A's on a route
// that names no object, and — for a route naming A's object — the SAME answer
// it gives about the ghost.
func (w *isoWorld) checkOutsider(t *testing.T, who string, results []isoResult) {
	t.Helper()
	for _, res := range results {
		named := namesObject(res.route)
		if l := w.leaks(res.body, !named); len(l) > 0 {
			t.Errorf("LEAK: %s %s %s → %d carries A's %v\n  body: %s", who, res.via, res.route, res.status, l, truncate(res.body, 300))
		}
		if named && res.status != res.ghost {
			t.Errorf("DISTINGUISHABLE: %s %s %s answered %d about A's object but %d about a ghost — it can tell A's "+
				"object exists (or acted on it)\n  body: %s", who, res.via, res.route, res.status, res.ghost, truncate(res.body, 300))
		}
		if strings.Contains(res.body, `"exists":true`) && strings.Contains(res.route, "/agent/connectors/{name}") {
			t.Errorf("LEAK: %s %s %s reports A's connector EXISTS: %s", who, res.via, res.route, truncate(res.body, 300))
		}
	}
}

func is2xx(code int) bool { return code >= 200 && code < 300 }

// TestNoRouteLetsOneUserReachAnothersData is the walk. See the file header.
func TestNoRouteLetsOneUserReachAnothersData(t *testing.T) {
	w := newIsoWorld(t)
	routes := sortedRoutes()
	before := w.snapshot()
	w.router.reset()

	// --- user B, a member, through every one of B's credentials ---
	bResults := w.walkAs(w.b, w.objA, []cred{{sessionUser: w.b.uid}, {gotrueUser: w.b.uid}, {agentToken: w.b.token},
		{apiToken: w.apiTokens[w.b.uid]}, {runnerToken: w.runnerTokens[w.b.uid]}, {pluginToken: w.pluginTokens[w.b.uid]},
		{deviceToken: w.deviceTokens[w.b.uid]}}, routes)
	w.checkOutsider(t, "B", bResults)
	// The operator's own data (the router's directory list) is not A's, so the
	// canary check above cannot see it: a MEMBER must not get it either.
	for _, res := range bResults {
		if strings.Contains(res.body, w.opCanary) {
			t.Errorf("LEAK: B %s %s → %d carries the OPERATOR's data\n  body: %s", res.via, res.route, res.status, truncate(res.body, 300))
		}
	}
	// The gotrue leg must have been ADMITTED, or its clean bodies are 401s.
	gtAdmitted := 0
	for _, res := range bResults {
		if strings.HasPrefix(res.via, "gotrue-session") && is2xx(res.status) {
			gtAdmitted++
		}
	}
	if gtAdmitted < 20 {
		t.Errorf("CONTROL: B's real gotrue session was admitted on only %d routes — the leg is not walking", gtAdmitted)
	}
	// The personal-token leg must have been ADMITTED too (accounts PR 6): every
	// hook-tier and token route, or its clean bodies are 401s.
	tokAdmitted := 0
	for _, res := range bResults {
		if strings.HasPrefix(res.via, "api-token") && is2xx(res.status) {
			tokAdmitted++
		}
	}
	if tokAdmitted < 8 {
		t.Errorf("CONTROL: B's personal API token was admitted on only %d routes — the leg is not walking", tokAdmitted)
	}
	// The plugin-token leg too: B's plugin must have been ADMITTED on its own
	// door (its message stored as B's, its claim answered "nothing for you"),
	// or its clean answers everywhere else are only the refusal of an unknown
	// token. The result route names A's draft, so it is NOT among them.
	plugAdmitted := 0
	for _, res := range bResults {
		if strings.HasPrefix(res.via, "plugin-token") && is2xx(res.status) && isolationClasses[res.route] != classPublic {
			plugAdmitted++
			if isolationClasses[res.route] != classPlugin {
				t.Errorf("B's plugin token was answered %d on %s, which is not the plugin door", res.status, res.route)
			}
		}
	}
	if plugAdmitted != 2 {
		t.Errorf("CONTROL: B's plugin token was admitted on %d routes; want exactly its own message and claim routes", plugAdmitted)
	}
	// The device-credential leg: B's device must have been ADMITTED on its own
	// tier (its config read, its own unpair) and nowhere else. The push PUT is
	// a 400 (the walk's body is no subscription) and the push DELETE runs after
	// the unpair, so neither is among them.
	devAdmitted := 0
	for _, res := range bResults {
		if strings.HasPrefix(res.via, "device-token") && is2xx(res.status) && isolationClasses[res.route] != classPublic {
			devAdmitted++
			if isolationClasses[res.route] != classDevice {
				t.Errorf("B's device credential was answered %d on %s, which is not the device tier", res.status, res.route)
			}
		}
	}
	if devAdmitted != 2 {
		t.Errorf("CONTROL: B's device credential was admitted on %d routes; want exactly its config read and its own unpair", devAdmitted)
	}
	if len(bResults) < 5*len(isolationClasses) {
		t.Fatalf("the walk drove %d requests for %d routes", len(bResults), len(isolationClasses))
	}
	for _, res := range bResults {
		if res.route == "POST /agent/approvals" && strings.HasPrefix(res.via, "agent-token") && res.status != http.StatusServiceUnavailable {
			t.Errorf("B's (a member's) agent filing an approval: %d %s; want 503 → its own terminal", res.status, truncate(res.body, 200))
		}
	}
	w.checkCounts(t)
	w.srv.WaitForPushes()
	if got := w.router.snapshot(); len(got) != 0 {
		t.Errorf("a MEMBER's walk reached the operator's router %d time(s): %v", len(got), got)
	}
	if got := w.pusher.sentTo(w.aEndpoint); got != 0 {
		t.Errorf("B's walk delivered %d notification(s) to A's device", got)
	}
	// --- the mutation leg ---
	w.sameRows(t, "B's walk", before)
	// --- the connect leg ---
	if got := w.up.sawSecret(w.canary + "-SECRET"); got != 0 {
		t.Errorf("A's connector secret was injected upstream %d time(s) during B's walk", got)
	}
	if got := w.prov.actedOn(w.a.agent.ID, w.b.uid); got != 0 {
		t.Errorf("the provisioner acted on A's agent %d time(s) for B", got)
	}

	// --- the admin leg: user 1 (session, machine token, own agent) against member A ---
	// The admin-only routes are NOT in the admin's walk: there the admin acting
	// on A is the feature, and its writes would disable A mid-test. Their
	// admin control is TestAMemberCannotReachAnyAdminRoute.
	var opRoutes []string
	for _, r := range routes {
		if isolationClasses[r] != classAdminOnly {
			opRoutes = append(opRoutes, r)
		}
	}
	opResults := w.walkAs(w.op, w.objA, []cred{{sessionUser: w.op.uid}, {hook: true}, {agentToken: w.op.token}}, opRoutes)
	var opOutsider []isoResult
	for _, res := range opResults {
		// The operator's own approval card is the admin agent's to read.
		if !strings.Contains(res.route, "/agent/approvals/{id}") {
			opOutsider = append(opOutsider, res)
		}
	}
	w.checkOutsider(t, "admin (user 1)", opOutsider)
	w.sameRows(t, "the admin walk", before)
	w.srv.WaitForPushes()
	if got := w.pusher.sentTo(w.aEndpoint); got != 0 {
		t.Errorf("the admin's walk delivered %d notification(s) to member A's device", got)
	}
	if got := w.prov.actedOn(w.a.agent.ID, w.op.uid); got != 0 {
		t.Errorf("the provisioner acted on member A's agent %d time(s) for the admin", got)
	}

	// --- the NEGATIVE CONTROL: the same walk as A must reach A's data ---
	aResults := w.walkAs(w.a, w.objA, []cred{{sessionUser: w.a.uid}, {gotrueUser: w.a.uid}, {agentToken: w.a.token}}, routes)
	// A's PERSONAL-TOKEN control runs on its own objects (objTok: a fresh task
	// and comment of A's), AFTER the walk above. 🔴 NOT folded into that walk:
	// the hook tier's DELETE /api/tasks/… sort before the session tier's
	// DELETE /tasks/…, so A's token would delete the very objects the session
	// control then has to find — and the session routes would read as
	// unreached (measured: two CONTROL failures). Its results join aResults,
	// so the reached/canary checks below cover it.
	objTok := w.objA
	objTok.task, objTok.comment = w.aTokTask.ID, w.aTokComment.ID
	var tokRoutes []string
	for _, r := range routes {
		if c := isolationClasses[r]; c == classAdmin || c == classAPIToken {
			tokRoutes = append(tokRoutes, r)
		}
	}
	aResults = append(aResults, w.walkAs(w.a, objTok, []cred{{apiToken: w.apiTokens[w.a.uid]}}, tokRoutes)...)
	// A's RUNNER control, last: A's runner claims A's second queued job (the
	// first was cancelled by A's session walk above), and its heartbeat and
	// result on A's first job answer differently from the ghost's.
	var runnerRoutes []string
	for _, r := range routes {
		if isolationClasses[r] == classRunner {
			runnerRoutes = append(runnerRoutes, r)
		}
	}
	aResults = append(aResults, w.walkAs(w.a, w.objA, []cred{{runnerToken: w.runnerTokens[w.a.uid]}}, runnerRoutes)...)
	// A's PLUGIN control: A's plugin token on the plugin door, so the result
	// route — which names A's draft — answers A differently from the ghost.
	var pluginRoutes []string
	for _, r := range routes {
		if isolationClasses[r] == classPlugin {
			pluginRoutes = append(pluginRoutes, r)
		}
	}
	aResults = append(aResults, w.walkAs(w.a, w.objA, []cred{{pluginToken: w.pluginTokens[w.a.uid]}}, pluginRoutes)...)
	// The push legs above are only facts about B and the admin if A's own walk
	// DID reach A's device (its "send test").
	w.srv.WaitForPushes()
	if w.pusher.sentTo(w.aEndpoint) == 0 {
		t.Error("CONTROL: A's own walk never delivered to A's device — the push assertions above are vacuous")
	}
	sawCanary := map[string]bool{}
	reached := map[string]bool{}
	distinguishes := func(res isoResult) bool {
		if res.status != res.ghost || len(w.leaks(res.body, true)) > 0 {
			return true
		}
		m, ok := controlMarkers[res.route]
		return ok && strings.Contains(res.body, m) && !strings.Contains(res.ghostBody, m)
	}
	for _, res := range aResults {
		if len(w.leaks(res.body, true)) > 0 {
			sawCanary[res.route] = true
		}
		if namesObject(res.route) && distinguishes(res) {
			reached[res.route] = true
		}
	}
	// The approval card is the ADMIN agent's (a member cannot file one), so its
	// control is the admin walk: user 1's own agent reaches its own card.
	for _, res := range opResults {
		if strings.Contains(res.route, "/agent/approvals/{id}") && distinguishes(res) {
			reached[res.route] = true
		}
	}
	for _, route := range routes {
		// 🔴 ADMIN-TIER ROUTES ARE NO LONGER EXEMPT (accounts PR 6): A now holds a
		// machine credential of its own — its personal token — so the hook tier
		// gets the same "A's own answer differs from the ghost's" control as
		// every other route, and B's token walk above is a fact about identity.
		if !namesObject(route) || reached[route] {
			continue
		}
		if isolationClasses[route] == classAdminOnly {
			continue // A is a member too; the admin's control is TestAMemberCannotReachAnyAdminRoute
		}
		if _, ok := sideEffectControlled[route]; ok {
			continue
		}
		if _, ok := echoOnly[route]; ok {
			continue
		}
		t.Errorf("CONTROL: no credential of A's distinguished A's OWN object from a ghost on %s (same status, no "+
			"canary, no marker) — so an outsider's identical answers there prove nothing", route)
	}
	// The gotrue leg's own control: A's REAL gotrue session sees A's canary.
	gtCanary := 0
	for _, res := range aResults {
		if strings.HasPrefix(res.via, "gotrue-session") && len(w.leaks(res.body, true)) > 0 {
			gtCanary++
		}
	}
	if gtCanary == 0 {
		t.Error("CONTROL: A's real gotrue session never saw A's own data, so B's clean gotrue walk proves nothing")
	}
	// The passkey delete, on FRESH sessions (sideEffectControlled): B's and A's
	// gotrue sessions both ended at POST /logout in their walks, before the
	// {id} routes sort, so the walk itself never put this route to GoTrue —
	// measured: a fake whose delete ignored the owner survived it. B first:
	// A's passkey id on B's own delete route must leave it in place.
	bAT, bRT, _ := w.gtFake.Mint(w.bSub)
	w.gtTokens[w.b.uid] = [2]string{bAT, bRT}
	bm, bp, bb := w.fill("POST /account/passkeys/{id}/delete", w.objA, w.b)
	if rec := w.do(w.h, bm, bp, bb, cred{gotrueUser: w.b.uid}); rec.Code != http.StatusNotFound {
		t.Errorf("B removing A's passkey: %d, want 404", rec.Code)
	}
	if got := len(w.gtFake.Passkeys(w.aSub)); got != 1 {
		t.Errorf("LEAK: B removed A's passkey (A has %d, want 1)", got)
	}
	freshAT, freshRT, _ := w.gtFake.Mint(w.aSub)
	w.gtTokens[w.a.uid] = [2]string{freshAT, freshRT}
	pm, pp, pb := w.fill("POST /account/passkeys/{id}/delete", w.objA, w.a)
	if rec := w.do(w.h, pm, pp, pb, cred{gotrueUser: w.a.uid}); rec.Code != http.StatusSeeOther {
		t.Errorf("CONTROL: A could not remove its OWN passkey: %d %s", rec.Code, truncate(rec.Body.String(), 200))
	}
	if got := len(w.gtFake.Passkeys(w.aSub)); got != 0 {
		t.Errorf("CONTROL: A's passkey delete left %d passkeys, so B's refused delete proves nothing", got)
	}
	for _, r := range wantCanaryForA {
		if !sawCanary[r] {
			t.Errorf("CONTROL: A did not see its own data on %s, so B's clean body there proves nothing", r)
		}
	}
	if len(reached) < 30 {
		t.Errorf("CONTROL: A's walk distinguished its own objects on only %d routes", len(reached))
	}
	// The ACTIVE-section control: A's panel names A's in-flight chief.
	if body := w.do(w.h, "GET", "/ui/notifications", "", cred{sessionUser: w.a.uid}).Body.String(); !strings.Contains(body, w.aChief.Name) {
		t.Errorf("CONTROL: A's notifications panel does not name A's in-flight %s, so B's clean panel proves nothing\n  body: %s",
			w.aChief.Name, truncate(body, 300))
	}
	// The side-effect controls.
	if w.up.sawSecret(w.canary+"-SECRET") == 0 {
		t.Error("CONTROL: A's own agent never got A's connector secret upstream — the connect leg proves nothing")
	}
	if got := w.prov.actedOn(w.a.agent.ID, w.a.uid); got < 3 {
		t.Errorf("CONTROL: the provisioner acted on A's agent %d time(s) for A; want start, stop and destroy", got)
	}
	if after := w.snapshot(); after["chat_sessions"] == before["chat_sessions"] {
		t.Error("CONTROL: A's own POST /agents/sessions/{sid}/read did not move read_at, so B's unchanged row proves nothing")
	}
	// The machine-token control: the hook tier reaches user 1's OWN task.
	if rec := w.do(w.h, "GET", "/api/tasks/"+strconv.FormatInt(w.op.task.ID, 10), "", cred{hook: true}); rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), w.opCanary) {
		t.Errorf("CONTROL: the hook token could not read user 1's own task: %d %s", rec.Code, truncate(rec.Body.String(), 200))
	}
}

// checkCounts: a COUNT aggregate must be the caller's own — nothing in its
// body names A, so only the number can show the leak. B's review count must
// equal B's own (from System's view of B's rows), A's must equal A's (the
// control: A has a ready-for-review task, so a scope that dropped A's rows
// would also show). The admin's vocabulary routes must show the operator's
// own tag and project (their leak check above looks for A's).
func (w *isoWorld) checkCounts(t *testing.T) {
	t.Helper()
	truth := func(u isoUser) int {
		var n int
		if err := w.pool.QueryRow(w.sys, `SELECT count(*) FROM notes WHERE owner_id=$1 AND status='ready_for_review'
			AND deleted_at IS NULL`, u.uid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, u := range []isoUser{w.b, w.a} {
		rec := w.do(w.h, "GET", "/ui/tasks/review-count", "", cred{sessionUser: u.uid})
		var body struct {
			Count int `json:"count"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Count != truth(u) {
			t.Errorf("COUNT: %s's review count = %s; want its own %d", u.name, rec.Body, truth(u))
		}
	}
	// The ACTIVE section: A's in-flight chief is on nobody else's panel (the
	// walk's identity check covers this too). A's own panel is the control,
	// asserted after the mutation legs — rendering it creates A's chat session.
	for _, c := range []struct {
		u    isoUser
		want bool
	}{{w.b, false}, {w.op, false}} {
		body := w.do(w.h, "GET", "/ui/notifications", "", cred{sessionUser: c.u.uid}).Body.String()
		if strings.Contains(body, w.aChief.Name) != c.want {
			t.Errorf("ACTIVE: %s's notifications panel names A's in-flight %s = %v; want %v\n  body: %s",
				c.u.name, w.aChief.Name, !c.want, c.want, truncate(body, 300))
		}
	}
	if truth(w.a) == 0 {
		t.Error("CONTROL: A has no ready-for-review task, so B's count proves nothing")
	}
	for _, rt := range []struct{ path, want string }{{"/api/tags", "isoop" + w.suffix}, {"/api/projects", "isoopproj" + w.suffix}} {
		if rec := w.do(w.h, "GET", rt.path, "", cred{hook: true}); !strings.Contains(rec.Body.String(), rt.want) {
			t.Errorf("CONTROL: the admin's %s does not show the operator's own %q: %s", rt.path, rt.want, truncate(rec.Body.String(), 200))
		}
	}
}

// sameRows asserts every row A owns is unchanged since before.
func (w *isoWorld) sameRows(t *testing.T, what string, before map[string]string) {
	t.Helper()
	after := w.snapshot()
	for tbl, js := range before {
		if after[tbl] != js {
			t.Errorf("MUTATION: %s changed A's %s rows.\n before: %s\n  after: %s", what, tbl, truncate(js, 400), truncate(after[tbl], 400))
		}
	}
}

// --- the SSE leg -------------------------------------------------------------

// TestAnotherUsersWritesNeverReachYourEventStream: B holds /events open while
// A writes through the API (create, edit, comment, status) and A's agent's
// state changes; within 2s B's stream carries none of A's events (by A's task
// ids and agent name), while A's own stream does — and B's own write reaches B.
func TestAnotherUsersWritesNeverReachYourEventStream(t *testing.T) {
	w := newIsoWorld(t)
	ts := httptest.NewServer(w.h)
	t.Cleanup(ts.Close)

	open := func(uid int64) (*isoStream, func()) {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/events", nil)
		req.Header.Set("Cookie", sessionCookieFor(w.srv))
		req.Header.Set(isoUserHeader, strconv.FormatInt(uid, 10))
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("open /events as %d: %v %v", uid, err, resp)
		}
		st := &isoStream{}
		go st.read(resp.Body)
		return st, func() { cancel(); _ = resp.Body.Close() }
	}
	bStream, closeB := open(w.b.uid)
	defer closeB()
	aStream, closeA := open(w.a.uid)
	defer closeA()
	deadline := time.Now().Add(2 * time.Second)
	for w.bus.Count() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	asA := cred{sessionUser: w.a.uid}
	aTask := strconv.FormatInt(w.a.task.ID, 10)
	for _, rq := range []struct{ m, p, b string }{
		{"POST", "/tasks", "multipart:" + url.Values{"title": {w.canary + " new"}}.Encode()},
		{"POST", "/tasks/" + aTask + "/comments", url.Values{"body": {w.canary + " c2"}}.Encode()},
		{"PATCH", "/tasks/" + aTask + "/status", url.Values{"status": {"in_progress"}}.Encode()},
		{"POST", "/tasks/" + aTask + "/tags", url.Values{"tags": {"ssetag"}}.Encode()},
	} {
		if rec := w.do(w.h, rq.m, rq.p, rq.b, asA); rec.Code >= 400 {
			t.Fatalf("A's write %s %s: %d %s", rq.m, rq.p, rec.Code, rec.Body)
		}
	}
	w.srv.BroadcastAgentChanged(w.a.agent.Name) // a loop's nudge about A's agent
	// And B's own write, so B's stream is shown live.
	if rec := w.do(w.h, "PATCH", "/tasks/"+strconv.FormatInt(w.b.task.ID, 10)+"/status",
		url.Values{"status": {"in_progress"}}.Encode(), cred{sessionUser: w.b.uid}); rec.Code >= 400 {
		t.Fatalf("B's own write: %d %s", rec.Code, rec.Body)
	}
	time.Sleep(2 * time.Second)

	aGot, bGot := aStream.text(), bStream.text()
	for _, needle := range []string{`"id":"` + aTask + `"`, w.a.agent.Name} {
		if strings.Contains(bGot, needle) {
			t.Errorf("LEAK: B's /events stream carried A's %q:\n%s", needle, bGot)
		}
		if !strings.Contains(aGot, needle) {
			t.Errorf("CONTROL: A's own stream never carried %q, so B's silence proves nothing:\n%s", needle, aGot)
		}
	}
	if !strings.Contains(bGot, `"id":"`+strconv.FormatInt(w.b.task.ID, 10)+`"`) {
		t.Errorf("CONTROL: B's stream never carried B's own event:\n%s", bGot)
	}
}

type isoStream struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *isoStream) read(r io.Reader) {
	b := make([]byte, 4096)
	for {
		n, err := r.Read(b)
		s.mu.Lock()
		s.buf.Write(b[:n])
		s.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (s *isoStream) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// --- fakes for the parts that are not this package's -------------------------

// isoMirror plays the mirror plugin as mirror.Service behaves: A's one host
// (named with the canary) is visible to A's principal only — Hosts and Attach
// act for ctx's principal; a scope-all or principal-less context has none.
// #63's own tests cover the real Service's owner keying; this is the API side.
type isoMirror struct {
	owner int64
	host  string
}

func (m *isoMirror) mine(ctx context.Context) bool {
	p, ok := authz.From(ctx)
	return ok && !p.All() && p.UserID == m.owner
}

func (m *isoMirror) ServeConnect(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "mirror token required", http.StatusUnauthorized)
}

func (m *isoMirror) Hosts(ctx context.Context) []mirror.HostView {
	if !m.mine(ctx) {
		return nil
	}
	return []mirror.HostView{{Host: m.host, Sessions: []string{"main"}, Online: true}}
}

func (m *isoMirror) AttachWindow(ctx context.Context, host, _, _ string, _ bool, _ io.Reader, stdout io.Writer, _ <-chan provision.TermSize) error {
	if !m.mine(ctx) || host != m.host {
		return mirror.ErrHostOffline
	}
	_, err := io.WriteString(stdout, "mirrored "+m.host)
	return err
}

// AnswerHermesApproval: A's own host has no such approval (a 409, which is
// still A reaching the route); anyone else's request never gets this far.
func (m *isoMirror) AnswerHermesApproval(ctx context.Context, host, _, _, _, _ string) error {
	if !m.mine(ctx) || host != m.host {
		return mirror.ErrHostOffline
	}
	return mirror.ErrHermesApprovalGone
}

// isoUpstream records the credentials the proxy injected.
type isoUpstream struct {
	srv  *httptest.Server
	port string
	mu   sync.Mutex
	auth []string
}

func newIsoUpstream(t *testing.T) *isoUpstream {
	u := &isoUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.auth = append(u.auth, r.Header.Get("Authorization"))
		u.mu.Unlock()
		_, _ = io.WriteString(w, "upstream ok")
	}))
	t.Cleanup(u.srv.Close)
	u.port = u.srv.URL[strings.LastIndexByte(u.srv.URL, ':')+1:]
	return u
}

func (u *isoUpstream) sawSecret(secret string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, a := range u.auth {
		if strings.Contains(a, secret) || strings.Contains(decodeBasic(a), secret) {
			n++
		}
	}
	return n
}

func decodeBasic(h string) string {
	r := http.Request{Header: http.Header{"Authorization": {h}}}
	_, p, _ := r.BasicAuth()
	return p
}

// isoRouter is the operator's router: it records every push and mirror.
type isoRouter struct {
	stubLiveness
	dirs []string
	mu   sync.Mutex
	got  []string
}

func (r *isoRouter) PublishEvent(_ context.Context, name, data string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, "publish "+name+" "+data)
	return nil
}

func (r *isoRouter) Notify(_ context.Context, n RouterNotification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, "notify "+n.Title)
	return nil
}

func (r *isoRouter) Directories(context.Context, string, int) ([]string, error) { return r.dirs, nil }

func (r *isoRouter) SessionMeta(context.Context, string) (SessionMeta, bool, error) {
	return SessionMeta{}, false, nil
}

func (r *isoRouter) reset() {
	r.mu.Lock()
	r.got = nil
	r.mu.Unlock()
}

func (r *isoRouter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func (r *isoRouter) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

type isoGate struct {
	mu sync.Mutex
	n  int
}

func (g *isoGate) MintGate(context.Context, GateSpec) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return "iso-card-" + strconv.Itoa(g.n), nil
}
func (g *isoGate) ReadGate(context.Context, string) (GateDecision, error) {
	return GateDecision{State: GateStatePending}, nil
}
func (g *isoGate) ClearGate(context.Context, string) error { return nil }

// isoProvisioner stands in for the cluster. Like the real adapter it reads the
// agent row on the CALLER's context before acting, so a call for an agent the
// caller cannot see fails exactly as it would in production.
type isoProvisioner struct {
	store agents.Store
	mu    sync.Mutex
	acted map[[2]int64]int // (agent id, acting uid) -> successful actions
}

func (p *isoProvisioner) check(ctx context.Context, id int64) error {
	if _, err := p.store.Get(ctx, id); err != nil {
		return err
	}
	pr, _ := authz.From(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.acted == nil {
		p.acted = map[[2]int64]int{}
	}
	p.acted[[2]int64{id, pr.UserID}]++
	return nil
}

// actedOn waits briefly for the handlers' background actions, then reports
// how many times agent id was acted on for uid.
func (p *isoProvisioner) actedOn(id, uid int64) int {
	time.Sleep(200 * time.Millisecond)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.acted[[2]int64{id, uid}]
}
func (p *isoProvisioner) Dispatch(ctx context.Context, id int64, _ bool) error {
	return p.check(ctx, id)
}
func (p *isoProvisioner) Start(ctx context.Context, id int64) error   { return p.check(ctx, id) }
func (p *isoProvisioner) Stop(ctx context.Context, id int64) error    { return p.check(ctx, id) }
func (p *isoProvisioner) Destroy(ctx context.Context, id int64) error { return p.check(ctx, id) }
func (p *isoProvisioner) Instances(context.Context) ([]provision.Instance, error) {
	return nil, nil
}
func (p *isoProvisioner) StreamLogs(_ context.Context, a agents.Agent, emit func(string)) error {
	emit("log of " + a.Name)
	return nil
}
func (p *isoProvisioner) AttachTerminal(context.Context, agents.Agent, bool, io.Reader, io.Writer, <-chan provision.TermSize) error {
	return nil
}
func (p *isoProvisioner) KickoffUndeliverableReason() string { return "" }
func (p *isoProvisioner) Chat(context.Context, agents.Agent, string, string, func(string)) (string, error) {
	return "", nil
}
func (p *isoProvisioner) ChatWithTools(context.Context, agents.Agent, string, string, string, []agents.ToolDef, agents.ToolDispatch, agents.StreamEmit) (string, error) {
	return "", nil
}

// isoPusher is the walk's push.Pusher: it records which endpoints it was asked
// to reach and accepts everything, so "B's walk reached A's device" is a count.
type isoPusher struct {
	mu   sync.Mutex
	sent map[string]int
}

func (p *isoPusher) Send(_ context.Context, sub push.Subscription, _ []byte) push.SendResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sent == nil {
		p.sent = map[string]int{}
	}
	p.sent[sub.Endpoint]++
	return push.SendResult{StatusCode: http.StatusCreated}
}

func (p *isoPusher) sentTo(endpoint string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent[endpoint]
}
