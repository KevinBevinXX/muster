package api

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/authz"
)

// sessionCookieName is the cookie carrying the human session (see
// requireSession). It is HMAC-signed by internal/auth; the client cannot forge
// or extend one.
const sessionCookieName = "muster_session"

// sessionTTL is how long one login lasts. It is long because the alternative is
// an operator who stops using the phone surface: this is a self-hosted dashboard
// reached from a locked phone, not a bank. Revocation is by rotating
// MUSTER_UI_PASSWORD, which changes the signing key and invalidates every
// outstanding cookie at once — see uiSessionSecret.
const sessionTTL = 30 * 24 * time.Hour

// AuthConfig holds every auth tier this app performs.
//
// 🔴 THREE FIELDS FROM THE UPSTREAM SHAPE ARE ABSENT, AND THEIR ABSENCE IS A
// SECURITY PROPERTY RATHER THAN TIDYING. TerminalToken, TerminalUIWrites,
// TerminalUIHosts and ChiefToken all gated the terminal-WRITE surface — a
// free-form `send-keys` box onto an operator's real terminals. That surface is
// on the permission router's side of the carve in its entirety: muster
// registers no route that can reach an OPERATOR'S terminal, so a server that
// carried those credentials would be holding the keys to a door it does not
// have. Do not add them back "for parity"; a credential with no route is a
// credential that can only ever be leaked.
//
// ⚠ muster DOES NOW HAVE ONE TERMINAL ROUTE, AND IT NEEDS NONE OF THEM. A
// claude-code agent's live terminal (terminal.go, GET /agents/{name}/terminal
// and its socket) attaches to that AGENT'S own tmux session inside its own pod.
// It sits behind requireSession, the human tier, exactly as the agent chat
// socket does, and the hook token does not open it
// (TestTheTerminalSocketRefusesTheHookToken).
//
// HookToken protects the machine-called surface via a bearer token. It is
// enforce-when-set: an empty HookToken leaves those endpoints open, a
// back-compat debt to callers that predate the token.
//
// UIPassword is the HUMAN tier, and it is FAIL-CLOSED — the opposite polarity
// to HookToken, and that difference is the entire control. See
// BrowserAuthRefusal.
type AuthConfig struct {
	// HookToken, when non-empty, is required (Bearer or X-Muster-Token) on the
	// machine-called endpoints.
	HookToken string
	// ServiceToken gates the SERVICE-TO-SERVICE door: the credential a NAMED
	// sibling process presents, together with an X-Muster-Actor header naming
	// the caller it is acting for.
	//
	// 🔴 IT IS FAIL-CLOSED. The hook tier's enforce-when-set shape is a
	// back-compat debt to callers that predate the token; a door with no legacy
	// caller is owed none, so unset means 503 here. See ServiceWriteRefusal for
	// each refusal and the reason it exists, and requireServiceToken for the
	// trust this door widens.
	ServiceToken string
	// UIPassword is the shared operator secret that gates the HUMAN web tier.
	// FAIL-CLOSED: unset or too short means the browser surface refuses, it does
	// NOT mean the surface is open. See BrowserAuthRefusal.
	UIPassword string
	// SecureCookies marks the session cookie Secure. It is off by default because
	// a common path to this server is a plain-HTTP LAN port, and a Secure cookie
	// there is never sent — which would present as "login succeeds and then
	// nothing is logged in".
	SecureCookies bool
}

// hookTokenEnforced reports whether the hook endpoints require a token.
func (c AuthConfig) hookTokenEnforced() bool { return c.HookToken != "" }

// minServiceTokenLen is the shortest MUSTER_SERVICE_TOKEN this server will serve
// the service-to-service tier behind. Same floor and same reasoning as
// the other doors': `muster gentoken` mints
// auth.RandomToken(32), which base64-raw-url encodes to 43 characters, so this
// rejects only values nobody generated — a hand-typed placeholder committed to a
// manifest above all. It is a floor on the CONFIGURED secret, never on anything a
// request supplies, so it leaks nothing to a caller.
const minServiceTokenLen = 32

// ServiceWriteRefusal returns a human-readable reason this server will NOT serve
// the service-to-service tier, or "" when it will.
//
// 🔴 IT IS THE ChiefWriteRefusal SHAPE WITH TWO MORE CASES, AND IT IS FAIL-CLOSED
// FOR THE REASON HookArmingRefusal GIVES: "enforce-when-set" is a back-compat
// promise to callers that predate the token, and a door that has never shipped has
// no such caller. Inheriting the open default would give away a gate for nothing.
//
// The four refusals, in order:
//
//   - unset — the state a fresh deploy, a missing secret key and a typo'd env var
//     all land in. This is the state this change SHIPS in, on purpose: no manifest
//     sets MUSTER_SERVICE_TOKEN, so every route on this tier answers 503 until an
//     operator decides otherwise.
//   - too short — a placeholder rather than generated entropy.
//   - identical to HookToken — that token is ONE shared value, handed to every hook
//     script on both hosts and travelling in cleartext to the LAN NodePort many
//     times a day, so it NAMES NO CALLER. This door's whole content is an asserted
//     identity; admitting the secret every hook holds would let any of them assert
//     any name, which is a WRONG identity rather than a missing one.
//   - identical to UIPassword — 🔴 AND THIS ONE IS NOT PARANOIA. uiSessionSecret
//     DERIVES the browser session-cookie signing key from that password, so a
//     holder of a value that opened both doors could mint a signed operator session
//     for themselves: the service credential would silently become the human tier.
//
// That is boot-validated distinct from every other door this server has — two
// secrets plus the cookie key derived from the third.
//
// ⚠ TWO REFUSALS FROM THE UPSTREAM SHAPE ARE GONE BECAUSE THE CREDENTIALS THEY
// NAMED ARE GONE, NOT BECAUSE THE HAZARD WAS RE-JUDGED. They compared this
// token against the terminal and chief secrets, neither of which muster holds
// (see AuthConfig). If either credential is ever added here, its distinctness
// case has to come back with it — a field added without its refusal is a door
// that can silently collapse into another.
//
// ⚠ A "" RETURN IS NOT A CLAIM THAT A CALLER WILL BE ADMITTED. It says the server
// is willing to serve the tier; requireServiceToken still compares the presented
// credential AND still requires an attributable X-Muster-Actor, and it refuses if
// either fails. Configuration and admission are separate questions.
//
// The caller decides what to do with a non-empty reason: main() logs it at boot
// (unconditionally, in both directions — a silent healthy state and a silent
// refusing state are otherwise byte-identical in the log), and requireServiceToken
// answers 503.
func (c AuthConfig) ServiceWriteRefusal() string {
	switch {
	case c.ServiceToken == "":
		return "MUSTER_SERVICE_TOKEN is not set"
	case len(c.ServiceToken) < minServiceTokenLen:
		return fmt.Sprintf("MUSTER_SERVICE_TOKEN is shorter than %d characters (looks like a placeholder, not generated entropy)", minServiceTokenLen)
	case c.ServiceToken == c.HookToken:
		return "MUSTER_SERVICE_TOKEN is the same value as MUSTER_HOOK_TOKEN (one shared secret held by every hook script on both hosts, so it names no caller and every holder of it could assert any actor name at this door)"
	case c.ServiceToken == c.UIPassword:
		return "MUSTER_SERVICE_TOKEN is the same value as MUSTER_UI_PASSWORD (which derives the session-cookie signing key, so a holder of this credential could mint signed operator browser sessions)"
	}
	return ""
}

// minUIPasswordLen is the shortest MUSTER_UI_PASSWORD this server will gate
// the browser tier behind. It is a floor on the CONFIGURED secret, never on
// anything a request supplies, so it leaks nothing to a caller.
const minUIPasswordLen = 12

// BrowserAuthRefusal returns a human-readable reason this server will NOT serve
// the human web tier, or "" when it will.
//
// 🔴 IT IS FAIL-CLOSED, AND THAT POLARITY IS THE WHOLE POINT — it is the
// TerminalWriteRefusal shape, not the requireHookToken shape. An unset password
// must mean "refuse", never "serve to anyone", because "anyone" here is anyone
// who can reach a plain-HTTP LAN NodePort, and what they reach is the operator's
// session transcripts, tool inputs (file paths, bash command lines, edit
// bodies), and the approve/deny queue.
//
// ⚠ THIS PARAGRAPH USED TO END "and — through requireArmedTerminalUI — a
// free-form send-keys box", AND THE CLAUSE WAS INHERITED RATHER THAN TRUE. No
// such wrapper and no terminal-write surface onto the operator's terminals
// exists in this module: those routes stayed with the permission router in the
// carve. (An agent's own terminal is here now, behind this same tier; see
// AuthConfig.) The reasoning is
// unaffected — the transcripts and the approve/deny queue are on their own
// enough to make an open LAN port unacceptable — but the example was evidence
// for a hazard that is not on this side of the split, and evidence for the wrong
// service is what makes a reader stop checking.
//
// ⚠ THE COST, STATED PLAINLY: a deploy whose secret fails to mount serves NO web
// UI at all, rather than an open one. That is the intended trade. main() logs
// which state it is in at boot, unconditionally and in both directions, because
// a silent healthy state and a silent refusing state are otherwise identical in
// the log.
func (c AuthConfig) BrowserAuthRefusal() string {
	switch {
	case c.UIPassword == "":
		return "MUSTER_UI_PASSWORD is not set"
	case len(c.UIPassword) < minUIPasswordLen:
		return fmt.Sprintf("MUSTER_UI_PASSWORD is shorter than %d characters (looks like a placeholder, not a chosen secret)", minUIPasswordLen)
	}
	return ""
}

// uiSessionSecret derives the cookie-signing key from the operator password.
//
// 🔴 DERIVED RATHER THAN A SECOND SECRET, DELIBERATELY. One secret to provision
// is one secret to get wrong, and it buys the revocation story for free:
// rotating MUSTER_UI_PASSWORD changes this key, so every outstanding cookie
// stops verifying at once. A separate random signing key would instead survive a
// password rotation, which is the opposite of what "I rotated the password"
// means to an operator.
//
// The password is never used as a raw HMAC key: it is hashed with a domain
// separator first, so the key is fixed-width and this use cannot collide with
// any other use of the same string.
func (c AuthConfig) uiSessionSecret() []byte {
	sum := sha256.Sum256([]byte("muster-ui-session-v1\x00" + c.UIPassword))
	return sum[:]
}

// hasValidSession reports whether the request carries a cookie this server
// signed and that has not expired.
func (s *Server) hasValidSession(r *http.Request) bool {
	if s.auth.BrowserAuthRefusal() != "" {
		return false
	}
	ck, err := r.Cookie(sessionCookieName)
	if err != nil || ck.Value == "" {
		return false
	}
	_, err = auth.VerifySession(s.auth.uiSessionSecret(), ck.Value, time.Now())
	return err == nil
}

// requireSession gates the HUMAN web tier: the app shell, every panel, the
// session transcript pages, the approve/deny controls and the task surface.
//
// 🔴 IT USED TO BE A LITERAL `return next`, AND THAT IS WHAT THIS CLOSES. Human
// auth had been removed on the reasoning that the public path is fronted by
// Authelia and "the LAN is treated as trusted-open". The LAN half stopped being
// tenable once the session page began rendering tool INPUTS and carrying a
// free-form send-keys box: a NodePort bypasses the ingress entirely, so the
// Authelia edge is not on this path at all and never was.
//
// 🔴 THIS PARAGRAPH DESCRIBED A SECOND WRAPPER AND THREE ROUTES THAT DO NOT
// EXIST HERE, AND THE CORRECTION MATTERS MORE THAN THE DELETION. It read: "the
// routes that actually execute commands — POST /ui/term/send-keys,
// /ui/term/new-session, /ui/term/launch — are wrapped in requireArmedTerminalUI
// and DO NOT PASS THROUGH HERE." Enumerated over this module: no
// requireArmedTerminalUI is declared anywhere (its only occurrences were this
// comment and the one above it), and no /ui/term/* route is registered — the
// operator-terminal surface stayed with the permission router in the carve
// (the agent terminal in terminal.go is behind requireSession). The lesson
// the paragraph carried is real and is why the sentence is REPLACED rather than
// cut: gating requireSession is not the same as gating every browser surface,
// and a route registered outside it answers 200 to an anonymous caller. What
// pins that is TestEveryBrowserSurfaceRequiresAHumanSession, as a relationship
// over the route table rather than as spot checks — and that name is itself
// unported here, ledgered in internal/modulegate/citations_test.go under this
// file. Read the ledger before treating this citation as coverage.
//
// The refusal is shaped for the caller: a document navigation gets a 303 to the
// login page (so the operator lands somewhere useful), and anything else gets a
// 401 — which the shell's existing __cgAuthGate turns into a real navigation,
// and therefore into that same 303.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reason := s.auth.BrowserAuthRefusal(); reason != "" {
			// A person typing the address deserves the page that NAMES the
			// missing variable, not a JSON blob. /login renders that explanation
			// (and answers 503 itself), so the operator meeting this state reads
			// "no operator password is configured" rather than debugging a
			// password that was never wrong.
			if isDocumentNavigation(r) {
				http.Redirect(w, r, loginPath, http.StatusSeeOther)
				return
			}
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "the web UI is not configured for login (MUSTER_UI_PASSWORD)",
			})
			return
		}
		if s.hasValidSession(r) {
			s.admit(w, r, next, principalFor(sessionUserID(), authz.ViaSession))
			return
		}
		s.refuseUnauthenticated(w, r)
	}
}

// CrossOriginRefusal is the error requireSameOrigin answers.
const CrossOriginRefusal = "cross-origin request refused: this action must come from muster's own pages"

// requireSameOrigin refuses a state-changing request whose Origin (or, when the
// browser sent none, Referer) is not muster's own host (wsAllowedOrigins).
//
// 🔴 SameSite=Lax IS NOT A CSRF DEFENCE AGAINST A SIBLING SUBDOMAIN. "Site" is
// the registrable domain, so a page on any sibling subdomain of the one muster
// is served from is SAME-SITE: the browser attaches the session cookie to its
// cross-origin POST. ORIGIN, unlike site, includes the full host, and browsers
// send it on every cross-origin POST/PUT/PATCH/DELETE — htmx and fetch on
// muster's own pages send it too.
//
// 🔴 A REQUEST WITH NEITHER HEADER IS REFUSED. Every browser this app targets
// sends Origin on these methods, so a request carrying neither is not one of
// muster's own pages; the Referer fallback exists only for a privacy setting
// that strips Origin and keeps a same-origin Referer. A non-browser client of
// these routes (there is none: they are the Connectors tab's buttons) would
// have to send Origin itself.
//
// GET and HEAD pass: they change nothing (handleShare documents that rule).
func (s *Server) requireSameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || s.isSameOrigin(r) {
			next(w, r)
			return
		}
		s.logger.Printf("refused a cross-origin %s %s (Origin %q)", r.Method, r.URL.Path, r.Header.Get("Origin"))
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": CrossOriginRefusal})
	}
}

// isSameOrigin reports whether r's Origin — or Referer when Origin is absent —
// names one of muster's own hosts.
func (s *Server) isSameOrigin(r *http.Request) bool {
	src := strings.TrimSpace(r.Header.Get("Origin"))
	if src == "" {
		src = strings.TrimSpace(r.Header.Get("Referer"))
	}
	if src == "" || src == "null" {
		return false
	}
	u, err := url.Parse(src)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	for _, h := range s.wsAllowedOrigins(r) {
		if strings.EqualFold(u.Host, h) {
			return true
		}
	}
	return false
}

// refuseUnauthenticated writes the no-session refusal, choosing its shape from
// what the caller can actually act on.
//
// 🔴 A DOCUMENT NAVIGATION MUST NOT GET A BARE 401. The whole app is hx-boost +
// panel swaps, so most requests are XHR — but the first request of a session is
// a real navigation, and answering that with 401 shows the browser's own error
// page with no way forward. A 303 lands the operator on the login form. For the
// XHR case the 401 is correct and is what __cgAuthGate is already listening for.
func (s *Server) refuseUnauthenticated(w http.ResponseWriter, r *http.Request) {
	if isDocumentNavigation(r) {
		http.Redirect(w, r, loginPathFor(r), http.StatusSeeOther)
		return
	}
	s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "not signed in"})
}

// isDocumentNavigation reports whether this request is the browser fetching a
// top-level document, as opposed to an htmx/fetch/XHR call.
//
// It reads Sec-Fetch-Mode/Sec-Fetch-Dest where present (every browser this app
// targets sends them) and falls back to "asks for HTML and is not an htmx
// request". The htmx check is explicit because htmx sends Accept: text/html too,
// so Accept alone would classify every panel swap as a navigation and answer a
// redirect that htmx would happily swap into the panel.
func isDocumentNavigation(r *http.Request) bool {
	if r.Header.Get("HX-Request") != "" {
		return false
	}
	if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
		return false
	}
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" {
		return dest == "document"
	}
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// requireHookToken wraps a machine-called handler. When a HookToken is
// configured, the request must present it as `Authorization: Bearer <token>` or
// `X-Muster-Token: <token>` (constant-time compared).
//
// 🔴 WHEN NO HookToken IS CONFIGURED THIS WRAPPER ADMITS EVERYONE WHO CAN REACH
// THE PORT, AND ON THE LAN NodePort THAT IS ANYONE ON THE LAN. Say it that way
// round: an earlier version of this comment said the handler is "left open for
// back-compat", which describes the mechanism and hides the consequence. The
// primary path to this server is a plain-HTTP NodePort that bypasses the Authelia
// ingress entirely, so "open" here means every device on the house network can
// call every route on this tier — POST /api/send, the /api/response poll cycle,
// POST /api/tmux/snapshot — with no credential at all.
//
// The back-compat rationale is real and is kept: the hook scripts predate the
// token, and refusing them on an unconfigured server would break the highest-traffic
// operator-visible surface. It is a debt to EXISTING callers, not a house style —
// every other door in this file (BrowserAuthRefusal, TerminalWriteRefusal,
// ChiefWriteRefusal, ServiceWriteRefusal) is fail-closed, and HookArmingRefusal
// exists so a NEW route on this credential does not inherit the opening. main()
// warns at boot when the token is unset.
func (s *Server) requireHookToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// ⚠ THE UNENFORCED CASE ACTS AS THE MACHINE USER TOO. It admitted
		// everyone as the operator's own tier before principals existed; giving
		// it a principal states that rather than changing it.
		if !s.auth.hookTokenEnforced() || auth.ConstantTimeEqual(hookTokenFromRequest(r), s.auth.HookToken) {
			s.admit(w, r, next, principalFor(machineUserID(), authz.ViaHook))
			return
		}
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid or missing hook token"})
	}
}

// HookArmingRefusal reports why this server must not serve a FAIL-CLOSED hook-tier
// route, or "" when it may. Today there is one reason: no MUSTER_HOOK_TOKEN.
//
// 🔴 IT EXISTS BECAUSE "enforce-when-set" IS A BACK-COMPAT PROMISE, AND A ROUTE THAT
// HAS NEVER SHIPPED IS OWED NONE. requireHookToken leaves an unconfigured server
// OPEN so the hook scripts that predate the token keep working; that is a debt to
// EXISTING callers, not a house style. A brand-new route has no existing caller to
// break, so inheriting the open default would be giving away a gate for nothing in
// return — which is exactly what task #633's audit found: the agent-steering write
// moved from a gate that refused unconditionally (requireOperatorToken, since
// deleted with the operator tier in task #653 phase two) onto one that serves
// anybody when a variable is unset.
//
// ⚠ THE SHAPE IS THE ONE THE OTHER DOORS THAT MATTER ALREADY USE. BrowserAuthRefusal
// (the human tier), TerminalWriteRefusal and ChiefWriteRefusal are all fail-closed,
// and main.go states the reason in so many words: "an unset value refuses the web UI
// rather than serving it to anyone who can reach the LAN NodePort." The hook tier's
// openness is the exception here, not the rule.
func (c AuthConfig) HookArmingRefusal() string {
	if !c.hookTokenEnforced() {
		return "MUSTER_HOOK_TOKEN is not set, so this server cannot authenticate a machine caller"
	}
	return ""
}

// HookUnarmedField is the JSON key requireArmedHookToken sets to `true` on its
// 503, and the ONE thing a client may branch on to tell "this surface is not armed"
// from "this server is sick".
//
// 🔴 EXPORTED FOR THE REASON ServiceTierLogPrefix AND ChiefRefusalLogPrefix ARE: the
// string a consumer matches must be the string the code writes, stated once. The
// consumer is cmd/muster (unarmedHookSurface), and the two are pinned together
// by internal/api's TestTheAgentSteeringWriteRefusesAnUnarmedServer (the server
// writes it) and muster's TestTheUnarmedMarkerMatchesTheServer (the client
// reads the same spelling) — a second spelling is a spelling that rots.
const HookUnarmedField = "unarmed"

// requireArmedHookToken is requireHookToken's FAIL-CLOSED sibling: same credential,
// same constant-time comparison, but an unconfigured server REFUSES the route
// instead of serving it open.
//
// 🔴 USE IT FOR A MACHINE WRITE THAT STEERS SOMETHING, NOT FOR A READ. The
// distinction is the one the audit drew: reading an agent's transcript on an
// unarmed LAN server is a disclosure; telling an agent what to do is arbitrary work
// inside an agent instance — a shell, its repository checkout and whatever
// connectors it has been granted. Leaving the second
// one open when a variable happens to be unset is a strictly worse trade than the
// back-compat it would be inherited for.
//
// 🔴 IT ANSWERS 503, NOT 401, AND THE CODE IS THE MESSAGE. 401 tells an operator
// their credential is wrong and sends them to rotate a token that was never the
// problem; the server is refusing to serve the surface AT ALL. That is the same
// distinction requireTerminalToken and requireChiefToken already make, and
// muster already maps it to its own exit code (exitUnarmed, 9) so a script can
// tell "arm the server" from "fix your token" from "retry".
//
// ⚠ IT DOES NOT CHECK THE CREDENTIAL IN THE UNARMED CASE, and says so in the body:
// the caller's own token was not the problem and was not looked at.
//
// 🔴 THE BODY CARRIES HookUnarmedField, AND THAT IS WHAT THE CLIENT BRANCHES ON —
// NOT THE SENTENCE. Most 503s on this tier are genuine outages, so muster has
// to tell them apart to choose between exitUnarmed and exitServerError, and a
// discriminator made of prose is one a reword silently breaks. It is the same shape
// `queued:false` gives the approval gate (cmd/muster's refusedApproval).
func (s *Server) requireArmedHookToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reason := s.auth.HookArmingRefusal(); reason != "" {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":          "this server does not serve the machine agent-steering write: " + reason,
				HookUnarmedField: true,
			})
			return
		}
		if auth.ConstantTimeEqual(hookTokenFromRequest(r), s.auth.HookToken) {
			s.admit(w, r, next, principalFor(machineUserID(), authz.ViaHook))
			return
		}
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid or missing hook token"})
	}
}

// ServiceTierLogPrefix is the prefix every ATTRIBUTED WRITE admitted by
// requireServiceToken or requireHookOrServiceToken is logged with.
//
// 🔴 EXPORTED FOR THE SAME REASON ChiefRefusalLogPrefix IS: the string an operator
// greps for must be the string the code writes, stated once. A second spelling in a
// runbook is a spelling that rots.
//
// ⚠ IT REPLACES `AgentTierLogPrefix = "agent-tier: "`, WHICH IS A CHANGE TO WHAT AN
// OPERATOR GREPS FOR, and it is deliberate rather than cosmetic: nothing behind
// these two wrappers resolves an agent row any more, so a line calling itself
// "agent-tier" would name a mechanism that no longer exists. Both wrappers use this
// ONE constant because they are one tier with two admission shapes; a second
// spelling would make "which door admitted this write" a question of which prefix
// the author happened to reach for.
const ServiceTierLogPrefix = "service-tier: "

// ActorHeader is the header a service asserts its caller's name in.
//
// 🔴 IT IS A CONSTANT BECAUSE TWO WRAPPERS AND EVERY REFUSAL MESSAGE SPELL IT. A
// header name typed at four sites is a header name that is wrong at one of them,
// and the direction it would be wrong in is the quiet one: a misspelled read
// returns "", which is refused, so the surface would simply stop working for the
// one caller it exists for.
const ActorHeader = "X-Muster-Actor"

// serviceActorCtxKey carries the actor NAME requireServiceToken and
// requireHookOrServiceToken resolved. Private type, so no other package can plant
// one.
//
// 🔴 ONE KEY, NOT TWO, AND THAT IS LOAD-BEARING RATHER THAN TIDY. Both wrappers
// admit the same credential and assert the same identity, and two handlers read the
// value — handleAttentionRaise (to record raised_by) and requireAttentionOwnership
// (to check it). A second parallel key would mean those handlers saw an actor
// through one door and nothing through the other, which reads as "unattributed"
// and, for the ownership check, means "the operator's own tier" — i.e. the scoping
// silently inverted for every request through the second door.
type serviceActorCtxKey struct{}

// serviceActorFromContext returns the actor this request was attributed to, and
// whether it was attributed at all.
//
// 🔴 ok == false IS NOT AN ERROR. requireHookOrServiceToken admits two credential
// kinds and only one names a caller; a request on the shared hook token is
// legitimately unattributed, because that secret is held by every hook script on
// both hosts. Callers must decide what absence MEANS for them — and for the
// ownership check it means the operator's own tier, which is unscoped by design.
func serviceActorFromContext(ctx context.Context) (string, bool) {
	name, ok := ctx.Value(serviceActorCtxKey{}).(string)
	return name, ok
}

// admitAttributedService resolves the ASSERTED actor, logs an attributed write, and
// calls next — or refuses 403 when the request cannot be attributed.
//
// 🔴 IT IS ONE FUNCTION BECAUSE IT IS ONE RULE. Both service-tier wrappers need the
// identical three steps, and a predicate open-coded at two sites is a predicate that
// ends up wrong at one of them; here "wrong" means either a 403 that should have
// been an admission, or — far worse — an admission with no name on it.
//
// 🔴 READS ARE NOT LOGGED, ON PURPOSE. GET /api/tmux/snapshot is polled and every
// route on this tier is reachable from the LAN NodePort, so a per-request line is a
// log-flood primitive. isWriteRequest enumerates the SAFE methods, so a future verb
// is over-logged rather than silently unaudited.
func (s *Server) admitAttributedService(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	actor := strings.TrimSpace(r.Header.Get(ActorHeader))
	if !ValidActor(actor) {
		s.logger.Printf(ServiceTierLogPrefix+"refusing %s %s: %s is %q, which is not a legal actor "+
			"label, so the request could not be attributed", r.Method, r.URL.Path, ActorHeader, actor)
		s.writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "this request could not be attributed to a caller: " + ActorHeader +
				" is missing or is not a legal actor label",
		})
		return
	}
	if isWriteRequest(r) {
		s.logger.Printf(ServiceTierLogPrefix+"%s %s attributed to %q", r.Method, r.URL.Path, actor)
	}
	s.admit(w, r.WithContext(context.WithValue(r.Context(), serviceActorCtxKey{}, actor)), next,
		principalFor(machineUserID(), authz.ViaService))
}

// requireServiceToken gates the SERVICE-TO-SERVICE tier: a named sibling process
// presenting MUSTER_SERVICE_TOKEN plus an X-Muster-Actor header.
//
// 🔴 IT IS AN ASSERTED-IDENTITY MODEL, AND THAT IS A REAL TRUST WIDENING VERSUS THE
// ROW LOOKUP IT REPLACES — SAY IT PLAINLY. requireHookOrAgentToken used to resolve
// the presented bearer through Agents.GetByHooksToken, so the name on a write was a
// fact about the DATABASE. Here muster does not verify the actor; it BELIEVES a
// service that holds one secret when that service names its own caller. A
// compromised or buggy holder can write any name it likes into the audit log.
//
// ⚠ THE COMPENSATING FACTS, BOTH OF WHICH ARE MEASURABLE RATHER THAN REASSURING.
// (1) It is the same trust the hook token already grants UNATTRIBUTED: every holder
// of MUSTER_HOOK_TOKEN can already reach these surfaces with no name at all, so
// the change is from "authenticated and anonymous" to "authenticated and
// self-named", not from "verified" to "unverified". (2) THIS door costs no DB query
// at all: requireHookOrAgentToken spent one Agents.GetByHooksToken SELECT per refused
// request on a LAN-reachable route, and nothing here does. ⚠ THAT IS A CLAIM ABOUT
// THIS WRAPPER ONLY — requireHookOrServiceToken still performs that lookup for its
// third credential, so the round-trip is narrowed to one tier rather than removed
// from the process. Its order there puts both constant-time compares first, so the
// SELECT is reached only by a caller that presented neither configured secret.
//
// 🔴 CONFIGURATION FIRST, 503 AND NEVER 401. The caller's credential is not the
// problem when the server has no secret of its own, and a 401 sends an operator to
// rotate a token that was never wrong. This is the same distinction
// requireTerminalToken, requireChiefToken and requireArmedHookToken already make.
//
// ⚠ THE 503 BODY REUSES HookUnarmedField RATHER THAN DECLARING A SECOND MARKER, AND
// THAT IS A DECISION WITH A REASON. The exported-constant PATTERN is what matters —
// a client must branch on a machine-readable key, not on a sentence a reword breaks
// — but there is no consumer for this tier yet (muster has no HTTP client at all),
// and an unused `ServiceUnarmedField` would be a second spelling of "not armed" that
// nothing pins, i.e. the drift this package's constants exist to prevent. The field
// means "this SURFACE is not armed", which is true of both doors; when a real
// consumer needs to tell the two tiers apart, give it its own constant then.
//
// There is NO fallback to any other token: presenting a valid hook, terminal or
// chief credential here is a 401.
func (s *Server) requireServiceToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reason := s.auth.ServiceWriteRefusal(); reason != "" {
			// 🔴 THE REASON GOES TO THE LOG AND NOT TO THE WIRE. It used to be appended
			// to this body, and this refusal is written BEFORE the credential compare —
			// so every unauthenticated device that can reach the LAN NodePort was handed
			// the full configuration verdict: that the token "looks like a placeholder",
			// or that it collides with MUSTER_UI_PASSWORD, naming the variable AND
			// what it derives. No secret crosses, but a misconfiguration oracle does,
			// and this was the only one of the four doors that disclosed it:
			// requireTerminalToken and requireChiefToken answer a flat sentence, and
			// requireHookOrServiceToken already logs its reason instead of writing it.
			//
			// ⚠ HookUnarmedField STAYS. The sentence is now generic on purpose, which
			// makes the machine-readable marker the ONLY thing a client can branch on to
			// tell "this surface is not armed" from "this server is sick" — so removing
			// it while genericising the prose would have closed a leak by breaking the
			// discriminator.
			//
			// ⚠ IT LOGS PER REQUEST, UNLIKE requireTerminalToken, WHICH DELIBERATELY DOES
			// NOT. That door's header calls a per-request line on a LAN-reachable route a
			// log-flood primitive, and the concern is real here too — the unarmed state
			// is the state this change SHIPS in. It is accepted for the same reason
			// requireHookOrServiceToken accepts it one function up: without a line, the
			// reason exists nowhere a caller's arrival is recorded, and main()'s boot
			// banner cannot say that anyone actually tried.
			s.logger.Printf(ServiceTierLogPrefix+"refusing %s %s: this server does not serve the "+
				"service-to-service tier: %s", r.Method, r.URL.Path, reason)
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":          "this server does not serve the service-to-service tier",
				HookUnarmedField: true,
			})
			return
		}
		if !auth.ConstantTimeEqual(hookTokenFromRequest(r), s.auth.ServiceToken) {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid or missing service token"})
			return
		}
		s.admitAttributedService(w, r, next)
	}
}

// requireHookOrServiceToken admits THREE credentials: the server's shared hook
// token, unattributed; the service token together with an ASSERTED actor name; and
// the ONE agent row the server's MUSTER_CHIEF_TOKEN names, resolved through
// Agents.GetByHooksToken.
//
// 🔴 IT REPLACED HALF OF requireHookOrAgentToken AND ONLY HALF. That wrapper
// admitted (a) the shared hook token and (b) the ONE agent row MUSTER_CHIEF_TOKEN
// named. The extraction plan (§2.1) says to accept "the service token + asserted
// actor INSTEAD OF an agent-row lookup", i.e. add a way in that does not need the
// row. Half (a) MUST stay: the two real Claude Code shell hooks POST /api/attention
// on the shared secret (§2.2 — they are 100% staying), and removing it breaks the
// highest-traffic operator-visible surface on both hosts.
//
// 🔴 HALF (b) IS STILL HERE, AND ITS DELETION IS OWED TO PHASE 4 RATHER THAN DONE
// NOW. §4.4 does say the sixth door "deletes … the agent half of
// requireHookOrAgentToken outright", but that deletion is SEPARABLE from §2.1's
// actual requirement — that muster be ABLE to call these routes — which the service
// branch already satisfies. Deleting it in the same change is a privilege
// REMOVAL nobody asked for, and it was not hypothetical upstream: when that
// analysis was done the server's chief credential and the `agents` row named
// `chief` held the same value, and that pod was Running. Cutting
// this branch removes six capabilities from a pod that is running right now, while
// the agent's own instruction text still tells that pod it has them — and that
// text exists because "an agent that does not know this retries a refused route
// and reports an outage, which is exactly what happened".
//
// ⚠ THAT INSTRUCTION TEXT IS NOT IN THIS REPOSITORY, AND THIS COMMENT USED TO
// CITE IT BY FILENAME AS THOUGH IT WERE. It lives upstream, alongside the pod's
// helm values; muster carries neither. See the closing condition recorded in
// internal/provision — the seam that would hold muster's own version of it has
// no producer yet, which is precisely why the deletion cannot be sequenced from
// inside this repository alone.
//
// 🔴 AND THE STATED RE-GRANT PATH ARGUES AGAINST THE NEW DOOR'S OWN DESIGN. "Issue
// chief MUSTER_SERVICE_TOKEN" is the substitute §4.4 offers, but
// ServiceWriteRefusal's third refusal rejects the HOOK token here precisely because
// a widely-held secret "would let any of them assert any name". Putting the
// asserted-identity credential inside an LLM-driven pod with a repo checkout is that
// same collapse one step down: requireAttentionOwnership scopes on the ASSERTED
// name, so a holder could resolve another actor's entries. So the three branches
// stay until Phase 4 moves them together, and WHAT HAS TO MOVE TOGETHER IS NAMED:
// the agent's prompt (its route list), the rendering that supplies the pod's
// credential set, and the machine-readable statement of what the pod can reach.
//
// ⚠ ALL THREE ARE UPSTREAM, NOT HERE, AND NAMING THEM AS LOCAL SYMBOLS WAS A
// STALE CLAIM THIS COMMENT CARRIED THROUGH THE EXTRACTION. muster has no prompt
// text, no helm values and no capability manifest — `grep` finds none of the
// three. Deleting this branch without them is how the documented incident gets
// recreated on purpose, and the point of saying so HERE is that a reader of this
// file cannot discharge the condition from this repository.
//
// 🔴 THE ORDER IS CHEAP-FIRST, AND THE CHIEF NARROWING IS DELIBERATELY LAST. The two
// constant-time comparisons cost no I/O and are tried before the DB lookup, so an
// unauthenticated caller on the LAN NodePort cannot cause a SELECT with a value that
// was going to be admitted anyway. Within the row branch the chief checks sit AFTER
// the row lookup, and that ordering is load-bearing: comparing against ChiefToken
// first would be cheaper and would make the wrong-agent refusal UNREACHABLE for the
// case it exists to refuse — a valid, resolvable credential belonging to the WRONG
// agent would be rejected by the earlier comparison and this branch would never
// execute.
//
// ⚠ ENFORCE-WHEN-SET IS INHERITED FROM requireHookToken, NOT INTRODUCED, and it is
// kept EXACTLY as it was: with no hook token configured this serves everyone, so
// every fail-closed claim here is scoped to a server that has one — production's
// state. Changing that polarity is a separate decision with its own blast radius
// (see requireHookToken's header for what "open" means on the LAN NodePort).
//
// 🔴 THE EMPTY-TOKEN GUARD IS LOAD-BEARING TWICE OVER. The store resolves
// `WHERE hooks_token=$1` (agents/pgstore.go), so one row with an empty hooks_token
// would be returned for a request carrying NO credential — without this guard the
// tier is an open endpoint. It also gives an uncredentialed caller the refusal that
// NAMES the absence rather than the one about a token that is "neither".
//
// ⚠ ALL THREE BRANCHES PUT THEIR RESULT THROUGH THE ONE CONTEXT KEY
// (serviceActorCtxKey) — the hook branch by leaving it absent, the other two by
// setting the name they resolved. Two parallel keys would mean handleAttentionRaise
// and requireAttentionOwnership saw an actor through one door and nothing through
// another, which for the ownership check means "the operator's own tier", i.e. the
// scoping silently inverted. What differs between the two attributed branches is
// where the name COMES FROM: the row branch RESOLVES it from the database, the
// service branch BELIEVES the caller — see requireServiceToken for that widening
// stated in full.
func (s *Server) requireHookOrServiceToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.hookTokenEnforced() {
			s.admit(w, r, next, principalFor(machineUserID(), authz.ViaHook))
			return
		}
		token := hookTokenFromRequest(r)
		if auth.ConstantTimeEqual(token, s.auth.HookToken) {
			// The operator's own tier: legitimately unattributed, because that
			// secret is held by every hook script on both hosts and names nobody.
			// (Unattributed means no ACTOR name; it still acts as the machine
			// user, which is a different question.)
			s.admit(w, r, next, principalFor(machineUserID(), authz.ViaHook))
			return
		}
		if token == "" {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "no hook or service token was presented",
			})
			return
		}
		// BRANCH 2: the service credential. A constant-time compare and no I/O, so it
		// is tried before the row lookup below.
		serviceRefusal := s.auth.ServiceWriteRefusal()
		if serviceRefusal == "" && auth.ConstantTimeEqual(token, s.auth.ServiceToken) {
			s.admitAttributedService(w, r, next)
			return
		}
		// 🔴 BRANCH 3 IS DELETED HERE, AND ITS DELETION IS THE POINT OF THE MOVE
		// RATHER THAN A CASUALTY OF IT.
		//
		// Upstream this wrapper had a third branch admitting the ONE agent row a
		// dedicated token named: the credential was resolved to a row, narrowed to
		// that one agent, and the row's name became the asserted actor. It existed
		// so a privileged in-cluster agent could read and advance the task board
		// while the board lived on the other service, and the extraction plan
		// recorded its removal as owed to the phase that moved those routes.
		//
		// These ARE those routes. They moved, so the debt falls due here: an agent
		// that wants muster's board presents a service credential and asserts its
		// own name like any other caller, and muster holds no per-agent secret it
		// would have to resolve. What the branch cost while it existed was a
		// credential living inside an LLM-driven pod with a repository checkout,
		// carrying an asserted identity — and the refusal ladder guarding it was
		// four checks deep because a single missing one handed the tier to the
		// whole fleet.
		//
		// ⚠ WHAT THIS CHANGES FOR AN EXISTING CALLER, STATED RATHER THAN DISCOVERED:
		// a pod presenting its per-agent token to these two routes now gets a 401.
		// That is the intended end state, not a regression — but it is a BEHAVIOUR
		// CHANGE at the carve, and it belongs in the release note rather than only
		// in this comment.
		//
		// TestNoAgentRowCredentialIsAdmittedAtTheServiceTier pins the absence
		// structurally, so "add a row lookup back in" cannot be a quiet diff.
		if serviceRefusal != "" {
			// The unarmed case, and today it is EVERY case: no manifest sets
			// MUSTER_SERVICE_TOKEN. Reported distinctly from "wrong token" below
			// because with no secret configured there is no right token to hold —
			// telling a correctly-configured caller its credential was wrong sends
			// its operator to re-issue something that was never the problem.
			s.logger.Printf(ServiceTierLogPrefix+"refusing a non-hook credential: %s", serviceRefusal)
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "this server admits no service credential here: the service-to-service tier is not configured",
			})
			return
		}
		// Do not leak which half failed — the three are indistinguishable to a
		// caller by design.
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "the presented token is neither the hook token nor the service token",
		})
	}
}

// isWriteRequest reports whether a request changes state, for the audit log above.
//
// It is an enumeration of the SAFE methods rather than of the unsafe ones: a method
// nobody listed is treated as a write, so a future verb is over-logged rather than
// silently unaudited.
func isWriteRequest(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// hookTokenFromRequest extracts the presented hook token from either the
// Authorization: Bearer header or the X-Muster-Token header.
func hookTokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := cutBearer(h); ok {
			return after
		}
	}
	return strings.TrimSpace(r.Header.Get("X-Muster-Token"))
}

// cutBearer returns the token after a case-insensitive "Bearer " prefix.
func cutBearer(h string) (string, bool) {
	const prefix = "bearer "
	if len(h) >= len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):]), true
	}
	return "", false
}
