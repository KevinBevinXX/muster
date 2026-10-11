// Package metrics holds muster's Prometheus instrumentation. Everything here is
// registered on the default registry and exposed at /metrics.
//
// 🔴 THIS IS A SUBSET OF THE PACKAGE IT WAS CARVED FROM, AND THE OMISSIONS ARE
// THE POINT. The project muster was extracted from instrumented both products
// out of one file: permission decisions, Web Push delivery, auto-approve
// windows, the attention queue, suggestion generation and a transcript archive
// — none of which muster owns. Copying them would have shipped counters that
// can only ever read zero, and a metric that is structurally incapable of
// moving is worse than a missing one: it reads on a dashboard as "this
// subsystem is quiet" rather than "this subsystem is not here". Those metrics
// stayed with the routing service. If muster later grows one of those concerns,
// the counter arrives with it.
//
// 🔴 CARDINALITY RULE, APPLIED THROUGHOUT: every label below is drawn from a
// CLOSED Go vocabulary. No identifier that a producer can mint — a session id,
// a task id, an agent name, a free-form tag value, a raw request path — may
// ever become a label. Each one is an unbounded series and a leak of somebody
// else's identifier into a metrics store that is usually less protected than
// the database. Where that rule is easy to break by accident, the declaration
// says so at its own site.
package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// BuildInfo is a constant gauge carrying the running version as a label.
	BuildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "muster_build_info",
		Help: "Build info; value is always 1, the version is a label.",
	}, []string{"version"})

	httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_http_requests_total",
		Help: "HTTP requests by matched route pattern, method, and response status code.",
	}, []string{"route", "method", "code"})

	httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "muster_http_request_duration_seconds",
		Help:    "HTTP request duration by matched route pattern and method.",
		Buckets: []float64{0.005, 0.025, 0.1, 0.5, 1, 2.5, 5, 10},
	}, []string{"route", "method"})

	// Panics counts recovered panics (HTTP handlers + background goroutines) — a
	// nonzero value is always worth alerting on.
	Panics = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_panics_total",
		Help: "Recovered panics by source.",
	}, []string{"source"})

	// AgentProvision counts agent provisioning attempts.
	AgentProvision = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_agent_provision_total",
		Help: "Agent provision attempts by result (ok|failed).",
	}, []string{"result"})

	AgentProvisionDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "muster_agent_provision_duration_seconds",
		Help:    "Agent provision wall time, end to end.",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300},
	})

	// AgentKickoffResend counts kickoff recoveries after the container that
	// received the message died (out-of-memory kill, eviction, node drain), by
	// outcome:
	//   resent    — a re-send was launched against the replacement gateway
	//   exhausted — the re-send budget was spent and the agent was errored
	//
	// `exhausted` is the alertable one: it means a workspace is dying repeatedly
	// before it can start work — a memory limit too low for a real repository
	// clone is the shape that motivated this counter. A nonzero `resent` with no
	// `exhausted` is the system self-healing as designed.
	AgentKickoffResend = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_agent_kickoff_resend_total",
		Help: "Kickoff re-deliveries after the receiving container died (resent|exhausted).",
	}, []string{"result"})

	// AgentKickoffClaim counts outcomes of the CROSS-PROCESS kickoff claim — the
	// guard that stops two replicas from both kicking off one agent during a
	// rolling deploy:
	//   won                     — this process took the claim and ran the kickoff
	//   lost                    — another process held it; this kickoff was skipped
	//   error                   — the claim query failed; the kickoff was skipped
	//                             (FAIL CLOSED — the reconciler retries)
	//   renew_lost              — a renewal found the claim no longer ours, i.e.
	//                             a peer took it while our turn was still running
	//   renew_error             — a renewal round-trip failed (the claim is still
	//                             ours until the TTL; three in a row loses it)
	//   shutdown_released       — a claim released on the SIGTERM path
	//   shutdown_release_failed — that release errored; the claim waits out the TTL
	//   stale_swept             — a claim from a previous incarnation of this host
	//                             cleared at startup
	//
	// 🔴 THIS COUNTER MAY BE THE ONLY OBSERVABILITY THIS MECHANISM HAS. The log
	// lines beside each Inc() live in the process's stdout, which rolls away with
	// the pod on every deploy unless something is shipping logs. A deployment
	// that scrapes metrics but does not collect logs — a common and entirely
	// reasonable shape — can observe neither a duplicate-kickoff bug nor its fix
	// without this.
	//
	// `renew_lost` is the alertable one: it means two kickoffs for one agent are
	// genuinely running concurrently. `lost` is the mechanism working as designed
	// and should be rare-but-nonzero around deploys; a `lost` of exactly zero
	// forever is weak evidence the claim is never contended, not that it works.
	AgentKickoffClaim = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_agent_kickoff_claim_total",
		Help: "Cross-process kickoff claim outcomes (won|lost|error|renew_lost|renew_error|shutdown_released|shutdown_release_failed|stale_swept).",
	}, []string{"result"})

	// TasksCreated counts tasks created through the HTTP API, by their derived
	// producer source.
	//
	// 🔴 THE `source` LABEL IS BOUNDED BY A CLOSED ALLOWLIST IN THE API LAYER, not
	// by whatever the request carried. Deriving it from an Origin header or a
	// user agent directly would let any caller mint a new series.
	TasksCreated = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_tasks_created_total",
		Help: "Tasks created via the HTTP API, by derived source (a closed allowlist).",
	}, []string{"source"})

	// RoutingTags counts routing (reserved-namespace) tags written onto tasks.
	//
	// 🔴 CARDINALITY RULE: the ONLY label is the reserved NAMESPACE, drawn from
	// the closed Go allowlist notes.RoutingNamespaces(). A tag's VALUE and any
	// free-form tag must NEVER reach a Prometheus label: free-form tags are
	// user/producer text and can carry repository or project names, so they would
	// blow up the label space and leak identifiers into metrics.
	RoutingTags = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_task_routing_tags_total",
		Help: "Routing tags written onto tasks, by reserved namespace ONLY — never the tag value.",
	}, []string{"namespace"})

	// TaskSessionLinks counts task-thread links that CHANGED — a session joining
	// a task's thread, or its role being upgraded. A repeat touch that only moves
	// last_seen_at is NOT counted, so this measures thread growth rather than
	// hook traffic.
	//
	// 🔴 CARDINALITY RULE, same discipline as RoutingTags: the ONLY label is
	// `role`, drawn from the closed three-value vocabulary
	// notes.RoleRead|RoleWorked|RoleCreated. The SESSION ID must NEVER become a
	// label — it is unbounded producer-supplied text, one new series per session.
	TaskSessionLinks = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_task_session_links_total",
		Help: "Task-thread links created or upgraded, by role ONLY (read|worked|created) — never the session id.",
	}, []string{"role"})

	// TaskSessionLinksSkipped counts task-thread links that were NOT recorded, by
	// a closed two-value reason: `thread_full` (the task already carried
	// notes.MaxTaskSessions created/worked links, so a `read` breadcrumb was
	// dropped) or `error` (a store failure).
	//
	// 🔴 IT EXISTS BECAUSE THE FAILURE IS DELIBERATELY SILENT TO THE CALLER. The
	// cap is advisory — a provenance breadcrumb must never break the operation it
	// annotates — so a skipped link produces a normal 200. Without this counter
	// the only trace would be a log line, and "the thread quietly stopped
	// growing" would be indistinguishable from "nothing touched this task".
	TaskSessionLinksSkipped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muster_task_session_links_skipped_total",
		Help: "Task-thread links NOT recorded, by reason ONLY (thread_full|error) — never the session id.",
	}, []string{"reason"})
)

// ObserveHTTP records one HTTP request's matched route pattern, method, status,
// and duration.
//
// 🔴 `route` MUST BE THE ROUTE PATTERN, NEVER THE REQUEST PATH. The pattern
// ("GET /api/tasks/{id}") is drawn from the mux's own finite route table, so the
// label space is bounded by the size of that table. The raw path carries ids and
// sometimes tokens and is unbounded. Callers pass "unmatched" on the 404 path
// for exactly this reason.
func ObserveHTTP(route, method string, code int, d time.Duration) {
	httpRequests.WithLabelValues(route, method, strconv.Itoa(code)).Inc()
	httpDuration.WithLabelValues(route, method).Observe(d.Seconds())
}
