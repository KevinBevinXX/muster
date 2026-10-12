package metrics

import (
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// ---------------------------------------------------------------------------
// THE METRIC LEDGER.
//
// 🔴 THESE ARE INVARIANT GUARDS, NOT REGRESSION COVERAGE, and the label matters.
// No bug was ever observed in this file; what was observed, on the project this
// package was carved out of, is that instrumentation accretes — a counter here,
// a label there — and the two properties that matter are exactly the two nobody
// checks at the moment they add one:
//
//  1. the metric NAMESPACE. A metric named for the wrong service lands in the
//     same Prometheus as the other one's, under a name that looks like it, and
//     the confusion surfaces on a dashboard months later.
//  2. the LABEL SETS. Every label here is supposed to be drawn from a closed Go
//     vocabulary. A label whose values a producer can mint — a session id, an
//     agent name, a tag value — is one new time series per value, and nothing
//     at the call site says so.
//
// Both are ledgers: they fail when the set GROWS *or* SHRINKS, so a metric
// appearing without a decision reddens as loudly as one going missing.
//
// ⚠ WHAT THEY DO NOT SEE, stated so the name cannot be read as wider than the
// body: nothing here checks that the values PASSED to a label are bounded. A
// call site that writes a session id into `role` produces a metric with the
// right name, the right label and unbounded cardinality, and these tests are
// blind to it. That is the caller's discipline; these guards only pin the
// SHAPE.
// ---------------------------------------------------------------------------

// wantMetrics is every metric this package registers, with its label names.
//
// A histogram or counter with no labels has an empty slice — distinct from a
// missing entry, which is the failure.
var wantMetrics = map[string][]string{
	"muster_build_info":                       {"version"},
	"muster_http_requests_total":              {"code", "method", "route"},
	"muster_http_request_duration_seconds":    {"method", "route"},
	"muster_panics_total":                     {"source"},
	"muster_agent_provision_total":            {"result"},
	"muster_push_delivery_total":              {"result"},
	"muster_push_suppressed_total":            {"reason"},
	"muster_agent_provision_duration_seconds": {},
	"muster_agent_kickoff_resend_total":       {"result"},
	"muster_agent_kickoff_claim_total":        {"result"},
	"muster_tasks_created_total":              {"source"},
	"muster_task_routing_tags_total":          {"namespace"},
	"muster_task_session_links_total":         {"role"},
	"muster_task_session_links_skipped_total": {"reason"},
}

// 🔴 LABELS THAT MAY NEVER APPEAR, BY NAME. This is the denylist half: the
// ledger above catches a metric added without a decision, but a metric added
// WITH a decision can still carry a label nobody thought about. These are
// the identifier names this domain actually holds and would reach for first.
var forbiddenLabels = []string{"session_id", "sessionid", "agent", "agent_name", "task", "task_id", "tag", "path", "id"}

// gather reads the default registry, which is where promauto put everything in
// metrics.go at package init.
//
// A metric family only appears in a Gather() once it has at least one child, so
// every *Vec is touched below before the read. That is the positive control for
// the whole file: without it, an empty registry would satisfy "no forbidden
// labels" and the ledger would be reasoning about nothing.
func gather(t *testing.T) map[string][]string {
	t.Helper()

	BuildInfo.WithLabelValues("0.0.0-test")
	Panics.WithLabelValues("test")
	AgentProvision.WithLabelValues("ok")
	PushDelivery.WithLabelValues("ok")
	PushSuppressed.WithLabelValues("muted")
	AgentKickoffResend.WithLabelValues("resent")
	AgentKickoffClaim.WithLabelValues("won")
	TasksCreated.WithLabelValues("api")
	RoutingTags.WithLabelValues("gate")
	TaskSessionLinks.WithLabelValues("read")
	TaskSessionLinksSkipped.WithLabelValues("error")
	ObserveHTTP("GET /api/tasks/{id}", "GET", 200, 0)

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	got := map[string][]string{}
	for _, f := range families {
		name := f.GetName()
		if !strings.HasPrefix(name, "muster_") {
			// Go runtime and process collectors are registered by default and are
			// not this package's to ledger.
			continue
		}
		labels := []string{}
		if len(f.GetMetric()) > 0 {
			for _, lp := range f.GetMetric()[0].GetLabel() {
				labels = append(labels, lp.GetName())
			}
		}
		sort.Strings(labels)
		got[name] = labels
	}
	if len(got) == 0 {
		t.Fatal("the gather found no muster_* metric families at all — every assertion below " +
			"would be a confident pass over an empty registry")
	}
	return got
}

// TestMetricLedgerIsExact fails when a metric is added or removed, and when a
// metric's label set changes.
func TestMetricLedgerIsExact(t *testing.T) {
	got := gather(t)

	for name, wantLabels := range wantMetrics {
		gotLabels, ok := got[name]
		if !ok {
			t.Errorf("the ledger names %q but the registry does not carry it — either the metric "+
				"was removed (say so here) or it has no child yet and gather() must touch it", name)
			continue
		}
		want := append([]string(nil), wantLabels...)
		sort.Strings(want)
		if strings.Join(gotLabels, ",") != strings.Join(want, ",") {
			t.Errorf("%s labels = %v, ledger says %v — a label change is a cardinality change; "+
				"update the ledger deliberately", name, gotLabels, want)
		}
	}
	for name := range got {
		if _, ok := wantMetrics[name]; !ok {
			t.Errorf("the registry carries %q, which is not in the ledger. Every metric is a time "+
				"series somebody pays for; add it here with its labels, deliberately", name)
		}
	}
}

// TestNoMetricCarriesAnUnboundedLabelName is the denylist half.
func TestNoMetricCarriesAnUnboundedLabelName(t *testing.T) {
	got := gather(t)

	// POSITIVE CONTROL. The matcher must be able to say yes, or a clean sweep
	// below is indistinguishable from a matcher wired to nothing.
	if !isForbidden("session_id") {
		t.Fatal("positive control failed: isForbidden(\"session_id\") = false, so the sweep below " +
			"cannot refuse anything and its clean verdict means nothing")
	}
	if isForbidden("role") {
		t.Fatal("negative control failed: isForbidden(\"role\") = true, so the sweep would refuse " +
			"a label that is deliberately allowed")
	}

	for name, labels := range got {
		for _, l := range labels {
			if isForbidden(l) {
				t.Errorf("%s carries the label %q. Every label in this package must come from a "+
					"CLOSED Go vocabulary; this one names an identifier a producer mints, which is "+
					"one new time series per value and a leak of that identifier into a metrics "+
					"store", name, l)
			}
		}
	}
}

func isForbidden(label string) bool {
	l := strings.ToLower(label)
	for _, f := range forbiddenLabels {
		if l == f {
			return true
		}
	}
	return false
}
