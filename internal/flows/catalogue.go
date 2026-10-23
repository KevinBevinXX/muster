// Package flows is muster's BROWSER-FLOW queue: a closed catalogue of named
// flows, an owner-scoped job table (migration 0012), and the per-(owner, host)
// runner tokens a host-side runner drains it with.
//
// The shape, end to end:
//
//	owner (browser session, or a personal mst_ token) ── enqueue ──► flow_jobs (queued)
//	host runner (mfr_ runner token, outbound long-poll) ── claim ──► running (leased)
//	                                                    ── report ─► done | failed | needs_operator
//
// muster never reaches into a host and never holds a browser credential: the
// runner dials OUT, claims a job addressed to its own host, re-validates it
// against its OWN copy of the catalogue, runs a fixed argv, and reports.
//
// 🔴 THE CATALOGUE IS COMPILED IN, NOT API-WRITABLE. A flow is a name, a class
// and a parameter schema; nothing a caller sends can widen it. Every parameter
// has a length cap and a pattern, and unknown parameters are refused, so a
// job's params are data the runner hands to a fixed executable as argv — never
// a URL, a selector or a script.
//
// 🔴 WHO MAY ENQUEUE is decided in internal/api, not here: a browser session or
// the owner's personal API token. The shared hook and service tokens are
// refused by construction (the routes sit behind requireAPIToken, which admits
// only mst_ tokens), because the hook token acts as the admin and is held by
// every hook script on every host.
package flows

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Class is whether a flow only reads from the operator's accounts or writes
// to them. It decides what a lost lease means: a read is safe to run again, a
// write may already have been delivered and must go to a human.
type Class string

const (
	ClassRead  Class = "read"
	ClassWrite Class = "write"
)

// Param is one parameter a flow takes. Every value is a string on the wire
// and in the job row; the runner turns it into argv.
type Param struct {
	Name     string
	Required bool
	// MaxLen is in characters (runes), and is always set.
	MaxLen int
	// Pattern must match the WHOLE value. Nil means "free text": any printable
	// characters, newlines and tabs, no other control characters.
	Pattern *regexp.Regexp
	// Help is the one-line description the /flows form and the CLI show.
	Help string
	// ID marks a CLOSED identifier (a task id: digits only, by its Pattern). It
	// is the one kind of parameter that is not prose somebody composed, so it
	// is the one kind the agent-text screen (requests.go) skips. Every other
	// parameter — whatever its pattern — is screened; a new parameter is
	// therefore screened by default.
	ID bool
	// Max, when set, is an inclusive NUMERIC upper bound: the value (which its
	// Pattern must already confine to decimal digits) may not exceed it. A
	// pattern alone cannot say "at most 3600" without spelling every range.
	Max int
}

// Flow is one catalogue entry.
type Flow struct {
	Name   string
	Class  Class
	Params []Param
	// Help is the one-line description of what the flow does.
	Help string
}

// Param returns the named parameter's spec.
func (f Flow) Param(name string) (Param, bool) {
	for _, p := range f.Params {
		if p.Name == name {
			return p, true
		}
	}
	return Param{}, false
}

var (
	// taskRE is a muster task id. The runner hands it to the flow as --task, so
	// the flow posts its result on that task.
	taskRE = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)
	// threadRE is a Muse side-chat's CLEANED title — the routing contract the
	// muse wrapper uses (exactly one match). One line, printable, no shell-ish
	// leading dash (it would read as a flag to the flow executable).
	threadRE = regexp.MustCompile(`^[^\x00-\x1f\x7f-][^\x00-\x1f\x7f]{0,119}$`)
	// sessionRE is a Muse thread's uuid, lowercase canonical: the muse wrapper
	// opens that thread directly rather than matching a title.
	sessionRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// markerRE is a dispatch id (muse dispatch): the send completes only when
	// the reply's last line is `DONE <marker>`.
	markerRE = regexp.MustCompile(`^md-[0-9a-f]{6}$`)
	// waitRE is the seconds a send awaits its reply: 1..9999 by shape, and
	// paramWait.Max bounds it to 3600.
	waitRE = regexp.MustCompile(`^[1-9][0-9]{0,3}$`)
)

// MaxWaitSeconds bounds muse.send's wait. The runner's job timeout is
// wait+240s, and it heartbeats inside the lease the whole time.
const MaxWaitSeconds = 3600

var (
	paramTask   = Param{Name: "task", MaxLen: 18, Pattern: taskRE, ID: true, Help: "muster task id the flow posts its result on"}
	paramThread = Param{Name: "thread", MaxLen: 120, Pattern: threadRE, Help: "Muse side-chat title (default: the main chat)"}
	// session, marker and wait (muse dispatch) are NOT ID: they stay inside the
	// agent-text screen. The dispatch client enqueues with a personal token, so
	// no consumer files them as an agent request, and the screen's long-token
	// rule refusing an agent-composed uuid is the closed default for that door.
	paramSession = Param{Name: "session", MaxLen: 36, Pattern: sessionRE,
		Help: "Muse thread id (uuid) — opens that thread directly; exclusive with thread"}
	paramMarker = Param{Name: "marker", MaxLen: 9, Pattern: markerRE,
		Help: "dispatch id; the send completes only when the reply's last line is DONE <marker>"}
	paramWait = Param{Name: "wait", MaxLen: 4, Pattern: waitRE, Max: MaxWaitSeconds,
		Help: "seconds to await the reply (1..3600; the runner's job timeout is wait+240s)"}
)

// catalogue is the closed set of flows muster will queue. The runner on the
// host holds its own copy (devrc scripts/muster-flow-runner) and refuses a job
// for a flow it does not list — two allowlists, one on each side of the trust
// boundary.
//
// ⚠ IT IS DELIBERATELY ONLY MUSE. Connectors and bring-your-own credentials
// cover any site with an API; a browser flow is for a site with no extractable
// credential, which today is Muse alone.
var catalogue = []Flow{
	{
		Name:   "muse.poll",
		Class:  ClassRead,
		Help:   "re-read the latest Muse reply (main chat, or a side chat)",
		Params: []Param{paramThread, paramTask},
	},
	{
		Name:  "muse.send",
		Class: ClassWrite,
		Help:  "send a message to Muse and wait for its reply",
		Params: []Param{
			{Name: "text", Required: true, MaxLen: 4000, Help: "the message (sent to Meta's Muse)"},
			paramThread, paramSession, paramMarker, paramWait, paramTask,
		},
	},
}

// Catalogue returns every flow, sorted by name.
func Catalogue() []Flow {
	out := append([]Flow(nil), catalogue...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup returns the named flow.
func Lookup(name string) (Flow, bool) {
	for _, f := range catalogue {
		if f.Name == name {
			return f, true
		}
	}
	return Flow{}, false
}

// ErrInvalid wraps every validation refusal: the caller answers 400 with its
// message.
var ErrInvalid = errors.New("invalid flow request")

func invalidf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

var (
	// hostRE is a runner's host label (the browser bridge's `whoami` label),
	// the same shape as a mirror host and the 0012 CHECK.
	hostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// instanceRE is a browser-bridge instance label (a Brave profile), the
	// same shape as the 0012 CHECK.
	instanceRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
)

// ValidHost reports whether host is a legal runner host label.
func ValidHost(host string) error {
	if !hostRE.MatchString(host) {
		return invalidf("host %q is not a host label (lowercase letters, digits and '-')", host)
	}
	return nil
}

// ValidInstance reports whether instance is a legal browser instance label.
func ValidInstance(instance string) error {
	if !instanceRE.MatchString(instance) {
		return invalidf("instance %q is not an instance label (lowercase letters, digits, '_' and '-')", instance)
	}
	return nil
}

// TTL bounds. A queued job not claimed within its TTL becomes `expired`, but
// time it spends blocked behind another job on the same browser does not
// count, up to MaxTTLPause (90 min) per blocker (store.go). So a queued job
// runs within its TTL plus its blocked time; behind a chain of dispatches
// that can be hours. That is intended, because a dispatch chain is queued,
// not refused. A send composed for a specific moment (a Muse message meant
// for "now") should therefore be enqueued with a short TTL on an idle
// instance.
const (
	DefaultTTL = 15 * time.Minute
	MinTTL     = time.Minute
	MaxTTL     = 24 * time.Hour
)

// Request is what an enqueuer asks for, before validation.
type Request struct {
	Flow     string
	Host     string
	Instance string
	Params   map[string]string
	// TTL is how long the job may wait to be claimed; zero means DefaultTTL.
	TTL time.Duration
}

// Validate checks r against the catalogue and returns the flow it names, with
// r.Params normalised (an empty optional value is dropped). Every refusal
// wraps ErrInvalid.
func Validate(r *Request) (Flow, error) {
	f, ok := Lookup(r.Flow)
	if !ok {
		return Flow{}, invalidf("unknown flow %q (the catalogue: %s)", r.Flow, strings.Join(Names(), ", "))
	}
	if err := ValidHost(r.Host); err != nil {
		return Flow{}, err
	}
	if err := ValidInstance(r.Instance); err != nil {
		return Flow{}, err
	}
	switch {
	case r.TTL == 0:
		r.TTL = DefaultTTL
	case r.TTL < MinTTL || r.TTL > MaxTTL:
		return Flow{}, invalidf("ttl %s is outside %s..%s", r.TTL, MinTTL, MaxTTL)
	}
	clean := map[string]string{}
	for name, v := range r.Params {
		p, ok := f.Param(name)
		if !ok {
			return Flow{}, invalidf("flow %s takes no parameter %q", f.Name, name)
		}
		if v == "" {
			continue
		}
		if err := checkValue(p, v); err != nil {
			return Flow{}, err
		}
		clean[name] = v
	}
	for _, p := range f.Params {
		if p.Required && clean[p.Name] == "" {
			return Flow{}, invalidf("flow %s needs parameter %q", f.Name, p.Name)
		}
	}
	// Cross-parameter rules. A thread is named by its title OR by its uuid,
	// never both (which one would win is a guess), and a dispatch marker always
	// targets the dedicated thread by uuid.
	if clean["session"] != "" && clean["thread"] != "" {
		return Flow{}, invalidf("parameters \"session\" and \"thread\" are exclusive; name the thread by one of them")
	}
	if clean["marker"] != "" && clean["session"] == "" {
		return Flow{}, invalidf("parameter \"marker\" needs \"session\" (a dispatch targets its thread by uuid)")
	}
	r.Params = clean
	return f, nil
}

func checkValue(p Param, v string) error {
	if !utf8.ValidString(v) {
		return invalidf("parameter %q is not valid UTF-8", p.Name)
	}
	if n := utf8.RuneCountInString(v); n > p.MaxLen {
		return invalidf("parameter %q is %d characters; the limit is %d", p.Name, n, p.MaxLen)
	}
	if p.Pattern != nil {
		if !p.Pattern.MatchString(v) {
			return invalidf("parameter %q has a value this flow does not accept", p.Name)
		}
		if p.Max > 0 {
			n, err := strconv.Atoi(v)
			if err != nil || n > p.Max {
				return invalidf("parameter %q is %s; the limit is %d", p.Name, v, p.Max)
			}
		}
		return nil
	}
	// Free text becomes ONE argv element of a fixed executable on the runner's
	// host, and muse's own parser has no `--`: a value beginning with '-' would
	// be read as a flag there (`--force` skips its pacing guard). The runner
	// refuses it too; this is the same rule on muster's side of the boundary.
	if strings.HasPrefix(v, "-") {
		return invalidf("parameter %q may not begin with '-'", p.Name)
	}
	for _, c := range v {
		if c == '\n' || c == '\t' {
			continue
		}
		if unicode.IsControl(c) {
			return invalidf("parameter %q carries a control character", p.Name)
		}
	}
	return nil
}

// Names lists the catalogue's flow names, sorted.
func Names() []string {
	var out []string
	for _, f := range Catalogue() {
		out = append(out, f.Name)
	}
	return out
}
