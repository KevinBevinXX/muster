package notes

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
)

// --- Tag grammar -------------------------------------------------------------
//
// Tags are a ROUTING KEY, not folders: a tag can drive behaviour (block
// dispatch, join an initiative), so it must be validated and FAIL
// LOUD rather than be a silently-ignored string. Normalization and validation are
// PURE functions here in the domain package so every write path (machine API, UI
// form, session route) shares one implementation and the store never accepts
// un-normalized input.
//
// Grammar (applied on every write path):
//   - lowercase; trim; internal whitespace collapses to '-'
//   - allowed charset [a-z0-9._/-], plus AT MOST ONE ':' separating namespace:value
//   - max MaxTagRunes runes per tag, max MaxTags tags per task
//   - de-duplicated, then sorted (deterministic render + stable tests)
//   - empty/whitespace-only tags are dropped SILENTLY; a tag that violates
//     charset/length is a VALIDATION ERROR, not a silent drop

const (
	// MaxTagRunes caps a single tag. Rune-based (not bytes) so the cap is stable
	// for non-ASCII input that normalization rejects anyway.
	MaxTagRunes = 64
	// MaxTags caps how many tags one task may carry.
	MaxTags = 20
)

// Reserved (routing) namespaces. These are a CLOSED Go allowlist on purpose:
// adding one is a code change, because each carries behaviour. Any other
// `namespace:value` prefix (e.g. `civitai:frontend`) is just a descriptive tag.
//
// ⚠ `runbook:` WAS ONE, AND IS NOT ANY MORE. Runbooks were removed (migration
// 0005 dropped their tables), so `runbook:<name>` carries no behaviour and is now
// an ordinary descriptive tag. Tasks written before the removal keep it as a
// plain label — nothing validates it against a store, routes on it or counts it.
const (
	// NSInitiative — `initiative:<slug>`: joins the task to the initiatives ledger.
	// SOFT validated (charset/length only): the initiatives store lives in a
	// different cluster's Postgres, and making task creation depend on a
	// cross-cluster DB would trade a real availability risk for a cosmetic check.
	NSInitiative = "initiative"
	// NSGate — `gate:<reason>`: the task is not dispatchable yet. Blocks dispatch.
	NSGate = "gate"
	// NSAuto — `auto:dispatch`: eligible for hands-off dispatch. PLUMBING ONLY,
	// disabled by default (MUSTER_TAG_AUTODISPATCH); behaves as a descriptive
	// tag while off.
	NSAuto = "auto"
	// NSProject — `project:<slug>`: which project the task belongs to. Authored
	// mostly by the capture extension. SOFT validated against no external store
	// (the vocabulary is derived FROM the tags themselves — a project exists
	// because a task names it), but with two rules the other namespaces don't
	// have, both enforced in the shared normalize/validate path:
	//   - the slug charset is NARROWER than the general tag charset
	//     ([a-z0-9][a-z0-9._-]* — no '/'), and normalization strips leading and
	//     trailing '-' so `Foo Bar` / `foo-bar` / `FOO   BAR` converge on ONE
	//     slug. Without that convergence the project list fragments into
	//     near-duplicates and the picker degrades into noise.
	//   - AT MOST ONE per task, because two makes every downstream "which project
	//     is this" query ambiguous.
	NSProject = "project"
)

// AutoDispatchTag is the exact tag that marks a task auto-dispatch eligible.
const AutoDispatchTag = NSAuto + ":dispatch"

// EXTERNAL-ID namespaces: a tag whose VALUE is an opaque identifier naming one
// specific thing, rather than a label shared by a group of tasks.
//
// 🔴 THE DISTINCTION IS WHAT MAKES THE FILTER ROW USABLE, AND IT WAS MEASURED.
// The live board carried 241 filter chips, 202 of them `clickup:<id>` — one per
// mirrored task, each matching exactly that one task. The row's scrollWidth was
// 33,967px against a 1,736px viewport (about twenty screens), with the scrollbar
// explicitly suppressed and the `›` cue pointer-events-none: on desktop there was
// no discoverable way to reach the far end at all. The ~39 chips that ARE filters
// were buried in it.
//
// A `clickup:<id>` chip is not a filter. Selecting it can only ever produce the
// single task that carries it — and you have to be looking at that task to know
// the id. It is a BACK-REFERENCE, which is what tagChip on the card already
// renders it as. So these namespaces are excluded from the filter chip ROW (an
// active one still gets a chip, or it could not be cleared) and left untouched
// everywhere else: still on the card, still in the editor, still filterable by
// URL.
//
// ⚠ WHY A NAMESPACE RULE RATHER THAN A COUNT. "Hide chips matching one task"
// would be a heuristic over today's data — it would hide a genuine label the day
// its second task is filed, and un-hide it the day after. Membership here is a
// property of the NAMESPACE's meaning and changes only when someone edits this
// list. Deliberately NOT routing and NOT reserved: neither drives behaviour and
// neither has a narrower grammar, so ValidateTags treats them as the ordinary
// descriptive tags they are.
const (
	// NSClickUp — `clickup:<id>`: the ClickUp task this one mirrors.
	NSClickUp = "clickup"
	// NSSupersededBy — `superseded-by:<taskID>`: stamped on the LOSER of a merge,
	// naming the winner. internal/api/merge.go writes it.
	NSSupersededBy = "superseded-by"
)

// externalIDNamespaces is the closed set. Closed for the same reason
// routingNamespaces is: adding one is a code change, because it removes a chip
// from a control.
//
// The ONE other source is the deployment's plugin task namespaces
// (SetPluginTaskNamespaces, from MUSTER_PLUGIN_TASK_NAMESPACES at boot): a key a
// plugin files tasks by names one upstream record, exactly like `clickup:<id>`.
// They are configuration rather than code because each names a deployment's own
// upstream, which this repository does not.
var externalIDNamespaces = map[string]bool{
	NSClickUp:      true,
	NSSupersededBy: true,
}

// pluginTaskNamespaces is the configured set, written once at boot (before the
// server serves) and read on every request; the atomic makes a test that sets
// it race-free with the reads it causes.
var pluginTaskNamespaces atomic.Pointer[map[string]bool]

// pluginTaskNamespaceRe is a namespace a deployment may configure: the tag
// charset, starting with a letter, with no '/' or '.' (a namespace, not a path).
var pluginTaskNamespaceRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// SetPluginTaskNamespaces validates and installs the deployment's plugin task
// namespaces, replacing any earlier set; nil clears it. A namespace that already
// means something — a routing or reserved one (`gate`, `auto`, `initiative`,
// `project`) or `superseded-by`, which only a merge writes — is refused, as is
// one that is not a plain namespace word.
func SetPluginTaskNamespaces(nss []string) error {
	if err := ValidatePluginTaskNamespaces(nss); err != nil {
		return err
	}
	set := make(map[string]bool, len(nss))
	for _, ns := range nss {
		set[ns] = true
	}
	pluginTaskNamespaces.Store(&set)
	return nil
}

// ValidatePluginTaskNamespaces is SetPluginTaskNamespaces's check alone, for the
// server's boot-time config validation.
func ValidatePluginTaskNamespaces(nss []string) error {
	for _, ns := range nss {
		switch {
		case !pluginTaskNamespaceRe.MatchString(ns):
			return fmt.Errorf("plugin task namespace %q must match %s", ns, pluginTaskNamespaceRe)
		case routingNamespaces[ns] || ns == NSProject || ns == NSSupersededBy:
			return fmt.Errorf("plugin task namespace %q is reserved: it already means something to muster", ns)
		}
	}
	return nil
}

// IsPluginTaskNamespace reports whether ns is one of the configured plugin task
// namespaces.
func IsPluginTaskNamespace(ns string) bool {
	set := pluginTaskNamespaces.Load()
	return set != nil && (*set)[ns]
}

// PluginTaskNamespaces returns the configured plugin task namespaces, sorted.
func PluginTaskNamespaces() []string {
	set := pluginTaskNamespaces.Load()
	if set == nil {
		return nil
	}
	out := make([]string, 0, len(*set))
	for ns := range *set {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// ExternalIDNamespaces returns the external-id namespaces — the closed set plus
// the configured plugin task namespaces — sorted, so a test or a renderer can
// enumerate them without reaching into this package's internals.
func ExternalIDNamespaces() []string {
	out := make([]string, 0, len(externalIDNamespaces))
	for ns := range externalIDNamespaces {
		out = append(out, ns)
	}
	for _, ns := range PluginTaskNamespaces() {
		if !externalIDNamespaces[ns] {
			out = append(out, ns)
		}
	}
	sort.Strings(out)
	return out
}

// IsExternalIDTag reports whether tag's namespace is an external-id one.
//
// It goes through ParseTag rather than a HasPrefix test so there is ONE
// definition of what a namespace is — a prefix test would also match a
// descriptive tag literally named `clickupX:…`.
func IsExternalIDTag(tag string) bool {
	ns, _ := ParseTag(tag)
	return externalIDNamespaces[ns] || IsPluginTaskNamespace(ns)
}

// routingNamespaces is the closed set of ROUTING namespaces. Membership is what
// makes a tag "routing" (behaviour-bearing) rather than descriptive.
//
// ⚠️ ROUTING ⊊ RESERVED. `project:` is reserved — it is a declared namespace with
// its own narrower grammar, enforced by ValidateTags — but it is deliberately NOT
// a member here, because it drives no behaviour: it classifies a task, it does not
// route one. That exclusion is load-bearing, not an oversight: membership is what
// puts a namespace into the Prometheus label space (observeRoutingTags), and a
// project vocabulary is exactly the unbounded repo-name-shaped cardinality that
// metric is designed to keep out. Pinned by TestProjectTagIsNotRouting and
// TestProjectValueNeverReachesAPrometheusLabel.
var routingNamespaces = map[string]bool{
	NSInitiative: true,
	NSGate:       true,
	NSAuto:       true,
}

// RoutingNamespaces returns the ROUTING namespaces, sorted — which is a strict
// SUBSET of the reserved ones (`project:` is reserved but not routing). It exists so
// telemetry/tests can enumerate the CLOSED allowlist without reaching into the
// package's internals — the same discipline as the taskSource allowlist that
// bounds Prometheus label cardinality.
func RoutingNamespaces() []string {
	out := make([]string, 0, len(routingNamespaces))
	for ns := range routingNamespaces {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// TagCount is one entry of the tag vocabulary: a tag and how many tasks carry it.
type TagCount struct {
	Tag   string `json:"tag"`
	Count int64  `json:"count"`
}

// ProjectCount is one entry of the project vocabulary: a project SLUG (without
// the `project:` prefix — that prefix is a storage detail, not something a picker
// should show) and how many tasks are in it.
type ProjectCount struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// ProjectsFromVocabulary derives the project vocabulary from the TAG vocabulary,
// so the two can never disagree about what "in use" or "a count" means. It is a
// PURE function, which is why the ordering contract below is unit-testable
// without a database.
//
// 🔑 COUNTING SEMANTICS (deliberate, inherited from TagVocabulary): a task counts
// toward its project REGARDLESS OF STATUS — including `complete`. Two reasons.
// (1) This endpoint feeds a PICKER; a project whose tasks are all finished is
// still a project you want to capture into, and dropping it would make the
// project vanish exactly when you finish a batch of work, forcing a re-type and
// re-fragmenting the vocabulary that normalization exists to keep converged.
// (2) `/api/tags`, the sibling vocabulary endpoint, has no status predicate
// either; two vocabulary endpoints that disagree would be a trap.
// Note there is no "dismissed" status to consider: dismissing a task SOFT-deletes
// it (the row and its comment thread survive, deleted_at is
// set), and every live read filters on that, so dismissed tasks are excluded
// structurally rather than by a predicate.
// Pinned by TestProjectsCountCompleteTasks.
//
// ORDERING: count DESC, then slug ASC. The sort is done HERE rather than relying
// on the store's ORDER BY, so the contract holds for every Store implementation
// (the in-memory fake and Postgres cannot drift apart on it).
func ProjectsFromVocabulary(vocab []TagCount) []ProjectCount {
	prefix := NSProject + ":"
	out := make([]ProjectCount, 0, len(vocab))
	for _, tc := range vocab {
		if !strings.HasPrefix(tc.Tag, prefix) {
			continue
		}
		name := strings.TrimPrefix(tc.Tag, prefix)
		if name == "" {
			continue
		}
		out = append(out, ProjectCount{Name: name, Count: tc.Count})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// NormalizeTag normalizes ONE raw tag: trim, lowercase, collapse internal
// whitespace runs to a single '-'. It does NOT validate — a normalized tag can
// still be invalid (bad charset, too long, multiple ':'), which ValidateTags
// reports loudly. An empty/whitespace-only input normalizes to "".
//
// `project:` slugs get ONE extra step: leading/trailing '-' are stripped. That is
// deliberately NOT applied to any other tag (a descriptive `-foo-` keeps today's
// exact behaviour), because the project vocabulary is a PICKER: `Foo Bar`,
// `foo-bar` and `FOO   BAR` have to converge on one slug or the list fragments
// into near-duplicates and the picker degrades into noise. A slug that is all
// dashes collapses to an empty value, which ValidateTags then rejects loudly
// rather than storing a meaningless `project:`.
func NormalizeTag(raw string) string {
	// strings.Fields splits on ANY unicode whitespace run, so joining with "-"
	// both trims the ends and collapses internal runs in one step.
	t := strings.ToLower(strings.Join(strings.Fields(raw), "-"))
	if ns, val := ParseTag(t); ns == NSProject {
		return NSProject + ":" + strings.Trim(val, "-")
	}
	return t
}

// NormalizeTags normalizes every tag, DROPS empty/whitespace-only entries
// silently, de-duplicates, and sorts. It is total (never errors) — validation is
// a separate, loud step so a caller can report exactly which tag was rejected.
// The result is always non-nil (an empty input yields an empty, non-nil slice) so
// it encodes as '{}' rather than SQL NULL on the NOT NULL tags column.
func NormalizeTags(raw []string) []string {
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		t := NormalizeTag(r)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// validTagRune reports whether r is in the allowed tag charset [a-z0-9._/-].
// ':' is handled separately (it is the single namespace separator).
func validTagRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return true
	case r == '.' || r == '_' || r == '/' || r == '-':
		return true
	default:
		return false
	}
}

// ValidateTags checks an ALREADY-NORMALIZED tag slice against the grammar and
// returns an error NAMING the offending tag (a routing tag that is silently
// ignored is the failure mode this whole feature has to avoid). It enforces the
// per-task count cap, the per-tag rune cap, the charset, and the at-most-one-':'
// rule. An empty tag at this point is a caller bug (NormalizeTags drops them), so
// it is reported rather than ignored.
func ValidateTags(tags []string) error {
	if len(tags) > MaxTags {
		return fmt.Errorf("too many tags: %d (max %d)", len(tags), MaxTags)
	}
	// projects collects every `project:` tag seen, so the at-most-one rule can
	// name BOTH offenders rather than just saying "too many".
	var projects []string
	for _, t := range tags {
		if t == "" {
			return fmt.Errorf("empty tag")
		}
		if n := len([]rune(t)); n > MaxTagRunes {
			return fmt.Errorf("tag %q is too long: %d characters (max %d)", truncTag(t), n, MaxTagRunes)
		}
		if strings.Count(t, ":") > 1 {
			return fmt.Errorf("tag %q has more than one ':' (expected at most namespace:value)", truncTag(t))
		}
		for _, r := range t {
			if r == ':' {
				continue
			}
			if !validTagRune(r) {
				return fmt.Errorf("tag %q contains an invalid character %q (allowed: a-z 0-9 . _ / - and one ':')", truncTag(t), string(r))
			}
		}
		// A bare ':' or a ':'-prefixed/suffixed tag has an empty half — that is a
		// malformed namespace, not a descriptive tag. This ALSO covers a project
		// slug that normalized to empty (e.g. `project:---`), so the slug check
		// below can assume a non-empty value.
		if i := strings.IndexByte(t, ':'); i >= 0 && (i == 0 || i == len(t)-1) {
			return fmt.Errorf("tag %q has an empty namespace or value", truncTag(t))
		}
		// `project:` slugs are narrower than the general tag charset: no '/', and
		// they must START with a letter or digit. See validateProjectSlug.
		if ns, val := ParseTag(t); ns == NSProject {
			if err := validateProjectSlug(t, val); err != nil {
				return err
			}
			projects = append(projects, t)
		}
	}
	// AT MOST ONE project per task. Two makes every downstream "which project is
	// this task in" query ambiguous, so it fails loudly NAMING BOTH values — a
	// producer that sent two needs to know which two to reconcile.
	if len(projects) > 1 {
		return fmt.Errorf("a task may carry at most one %s: tag, but got %q and %q", NSProject, truncTag(projects[0]), truncTag(projects[1]))
	}
	return nil
}

// validProjectSlugRune reports whether r is in the project slug charset
// [a-z0-9._-]. It is deliberately NARROWER than validTagRune: '/' is legal in a
// descriptive tag but would make a project slug look like a path, and the slug is
// the user-visible name in the picker and the filter control.
func validProjectSlugRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return true
	case r == '.' || r == '_' || r == '-':
		return true
	default:
		return false
	}
}

// validateProjectSlug enforces `[a-z0-9][a-z0-9._-]*` on an already-normalized,
// non-empty project slug (the empty case is caught by the empty-value rule in
// ValidateTags before this runs). Errors NAME the offending tag.
//
// There is deliberately NO slug-specific LENGTH rule: the pre-existing
// MaxTagRunes cap already bounds the whole tag at 64 runes, and `project:` costs
// 8 of them, so a slug is capped at 56. A second length rule would put one rule
// in two places and would be unreachable anyway. Pinned by
// TestProjectTagLengthBoundIsTheGeneralTagCap.
func validateProjectSlug(tag, slug string) error {
	first := []rune(slug)[0]
	if !(first >= 'a' && first <= 'z' || first >= '0' && first <= '9') {
		return fmt.Errorf("tag %q: a project slug must start with a letter or digit (a-z 0-9)", truncTag(tag))
	}
	for _, r := range slug {
		if !validProjectSlugRune(r) {
			return fmt.Errorf("tag %q contains %q, which is not allowed in a project slug (allowed: a-z 0-9 . _ -)", truncTag(tag), string(r))
		}
	}
	return nil
}

// truncTag bounds a tag embedded in an error message, so a 10k-rune payload can't
// produce a 10k-rune error string (and, transitively, a huge log line).
func truncTag(t string) string {
	const n = MaxTagRunes + 8
	if rs := []rune(t); len(rs) > n {
		return string(rs[:n]) + "…"
	}
	return t
}

// NormalizeAndValidate is the ONE entry point every write path uses: normalize
// (dropping empties, de-duping, sorting) then validate loudly. The returned slice
// is always non-nil on success.
func NormalizeAndValidate(raw []string) ([]string, error) {
	tags := NormalizeTags(raw)
	if err := ValidateTags(tags); err != nil {
		return nil, err
	}
	return tags, nil
}

// ParseTag splits a normalized tag into its namespace and value. A tag with no
// ':' is descriptive: namespace is "" and value is the whole tag.
func ParseTag(tag string) (namespace, value string) {
	if i := strings.IndexByte(tag, ':'); i > 0 {
		return tag[:i], tag[i+1:]
	}
	return "", tag
}

// IsRoutingTag reports whether tag sits in a RESERVED namespace — i.e. whether it
// is behaviour-bearing. An unrecognised `namespace:` prefix is descriptive.
func IsRoutingTag(tag string) bool {
	ns, _ := ParseTag(tag)
	return ns != "" && routingNamespaces[ns]
}

// RoutingTags returns only the routing (reserved-namespace) tags in tags, in
// input order.
func RoutingTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if IsRoutingTag(t) {
			out = append(out, t)
		}
	}
	return out
}

// TagValue returns the value of the FIRST tag in the given namespace, and whether
// one was present. Tags are sorted, so "first" is deterministic.
func TagValue(tags []string, namespace string) (string, bool) {
	for _, t := range tags {
		if ns, v := ParseTag(t); ns == namespace {
			return v, true
		}
	}
	return "", false
}

// ProjectName returns the `project:<slug>` the task belongs to, if any. There is
// AT MOST one (ValidateTags rejects two), so "the first" is "the only".
func ProjectName(tags []string) (string, bool) { return TagValue(tags, NSProject) }

// GateReason returns the `gate:<reason>` blocking reason, if the task is gated.
// A gated task is NOT dispatchable: the card's Dispatch is disabled and the
// dispatch endpoint refuses with 409.
func GateReason(tags []string) (string, bool) { return TagValue(tags, NSGate) }

// IsGated reports whether the task carries any `gate:` tag.
func IsGated(tags []string) bool {
	_, ok := GateReason(tags)
	return ok
}

// HasAutoDispatch reports whether the task carries the exact `auto:dispatch` tag.
// This is the RECOGNITION half of the auto-dispatch plumbing; whether it means
// anything is decided by the caller's feature flag (MUSTER_TAG_AUTODISPATCH) —
// see AutoDispatchEligible.
func HasAutoDispatch(tags []string) bool {
	for _, t := range tags {
		if t == AutoDispatchTag {
			return true
		}
	}
	return false
}

// AutoDispatchEligible reports whether a task is eligible for hands-off dispatch:
// the feature must be ENABLED, the task must carry `auto:dispatch`, and it must
// NOT be gated. Shipped OFF (enabled=false) — the manual dispatch loop has never
// once closed end-to-end, and automating a path that has never worked by hand
// converts an unproven feature into an unattended one. While the flag is off,
// `auto:dispatch` behaves exactly as a descriptive tag.
func AutoDispatchEligible(enabled bool, tags []string) bool {
	return enabled && HasAutoDispatch(tags) && !IsGated(tags)
}

// MergeTags returns the set-union of a and b, normalized/sorted (the pure
// counterpart of the store's single-statement AddTags). Used by the in-memory
// store and tests; the Postgres path does the union in SQL so two concurrent
// writers cannot lose an update.
func MergeTags(a, b []string) []string { return NormalizeTags(append(append([]string{}, a...), b...)) }

// SubtractTags returns a minus b (set difference), normalized/sorted — the pure
// counterpart of RemoveTags.
func SubtractTags(a, b []string) []string {
	drop := make(map[string]bool, len(b))
	for _, t := range NormalizeTags(b) {
		drop[t] = true
	}
	out := make([]string, 0, len(a))
	for _, t := range NormalizeTags(a) {
		if !drop[t] {
			out = append(out, t)
		}
	}
	return out
}

// HasAllTags reports whether n carries EVERY tag in want (the AND semantics of
// the `?tag=x&tag=y` filter). An empty want matches everything.
func HasAllTags(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	set := make(map[string]bool, len(have))
	for _, t := range have {
		set[t] = true
	}
	for _, t := range want {
		if !set[t] {
			return false
		}
	}
	return true
}
