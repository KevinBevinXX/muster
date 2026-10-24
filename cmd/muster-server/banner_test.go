package main

import (
	"bytes"
	"github.com/ZacxDev/muster/internal/plugins"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/router"
	"github.com/ZacxDev/muster/internal/uploads"
)

// ---------------------------------------------------------------------------
// THE BOOT BANNER IS A CLAIM, AND TWO OF ITS LINES WERE MEASURED WRONG.
//
// logBanner's own header promises "Each line below is printed whether the thing
// is on or off", because "a silent healthy state and a silent refusing state
// are otherwise byte-identical in the log". Two findings against that promise:
//
//  1. MUSTER_SECURE_COOKIES had NO line in either direction. It defaults FALSE,
//     so a TLS deployment that forgets it hands out a session cookie with no
//     Secure attribute and the banner says nothing at all.
//  2. The router line BLAMED THE WRONG VARIABLE. router.New returns nil when
//     base, token OR actor is empty, so `port.Configured()` is false for all
//     three — and the fallback branch reported "MUSTER_ROUTER_URL is unset" at
//     an operator who had set the URL and forgotten the token. Measured live,
//     with the URL set.
//
// 🔴 THE SECOND IS WORSE THAN A MISSING LINE, WHICH IS WHY IT GETS THE STRICTER
// ASSERTION. A missing line leaves the operator where they were; a line naming
// the wrong variable sends them to re-check the one that was already right.
// ---------------------------------------------------------------------------

// renderBanner runs logBanner against cfg with NO database pool.
func renderBanner(t *testing.T, cfg config, ext api.Extensions) string {
	t.Helper()
	return renderBannerWithPool(t, cfg, ext, nil)
}

// renderBannerWithPool runs logBanner and returns what it printed.
//
// 🔴 THE POOL IS A SEPARATE PARAMETER BECAUSE THE PERSISTENCE LINE BRANCHES ON
// IT, NOT ON cfg.Database — AND A FIXTURE THAT SET ONLY THE CONFIG LEFT THAT
// AXIS PINNED. Both renders took the "persistence: NONE" arm, so the
// both-directions check was comparing one branch against itself and would have
// passed however that line was written. Found by the differ-check below, which
// is the whole argument for it. A zero-value *pgxpool.Pool is enough: logBanner
// reads `a.pool != nil` and calls no method on it.
func renderBannerWithPool(t *testing.T, cfg config, ext api.Extensions, pool *pgxpool.Pool) string {
	t.Helper()
	var buf bytes.Buffer
	a := &app{cfg: cfg, logger: log.New(&buf, "", 0), pool: pool}
	port := router.NewPort(router.Config{
		BaseURL: cfg.RouterURL, Token: cfg.RouterToken, Actor: cfg.RouterActor,
	})
	a.logBanner(ext, port)
	out := buf.String()
	if strings.TrimSpace(out) == "" {
		t.Fatal("instrument check FAILED: logBanner printed NOTHING, so every assertion " +
			"below would be about an empty string")
	}
	return out
}

// routerLine returns the banner's "permission router:" line.
func routerLine(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "permission router:") {
			return line
		}
	}
	t.Fatalf("no \"permission router:\" line in the banner at all — logBanner's header "+
		"promises one in every direction.\nbanner:\n%s", out)
	return ""
}

// TestTheRouterBannerNamesTheVariableThatIsActuallyMissing is the F3 guard.
//
// 🔴 IT ASSERTS THE NEGATIVE TOO, AND THAT IS THE HALF THAT MATTERS. A check
// that the line MENTIONS MUSTER_ROUTER_TOKEN would pass against the old
// two-branch banner the moment anyone added the word anywhere; what was wrong
// was that it ALSO said the URL was unset while the URL was set. So each case
// pins both what must be named and what must not be claimed.
//
// 🔴 AND THE SUBSTRING VERSION OF THAT WAS SATISFIED BY THE BRANCH IT EXCLUDED.
// The configured case asserted `mustContain: {"CONFIGURED", url}` and
// `mustNotSay: {"is unset"}`. But "PARTIALLY CONFIGURED, THEREFORE NOT
// CONFIGURED" CONTAINS "CONFIGURED", and with all three variables set
// missingRouterCredential returns its case-0 sentence, which contains no "is
// unset" — so the partial branch satisfied every assertion of the configured
// case. Measured with an ordinary edit, not a sabotage: move the
// `case a.cfg.RouterURL != "":` arm above `case port.Configured():`, the sort of
// "group the router-URL cases together" tidy-up anybody might make, and a
// FULLY CONFIGURED router boots announcing PARTIALLY CONFIGURED, THEREFORE NOT
// CONFIGURED while the entire suite stays green. Same class as the defect this
// test was written for — the banner reporting the wrong router state — on the
// opposite axis.
//
// 🔴 SO THE WHOLE NORMALISED LINE IS PINNED. When the artifact under test IS
// PROSE, a guard on words is walkable by rewording, and a guard on a word that
// is a substring of its own negation is walkable by doing nothing at all. The
// cost is real and is accepted deliberately: a cosmetic reword of any line below
// fails this test and the wantLine has to be updated with it. That is the price
// of a machine-readable claim about a sentence.
//
// ⚠ mustContain/mustNotSay ARE KEPT ALONGSIDE IT, AND NOT AS BELT-AND-BRACES.
// A pinned line is walkable one way: regenerate the golden. Updating a wantLine
// is a reflex, and the reflex is exactly what a wrong line arrives as — so the
// negatives stay, because "the CONFIGURED line must not say PARTIALLY" is an
// INTENT that survives a reword and a careless re-pin alike.
func TestTheRouterBannerNamesTheVariableThatIsActuallyMissing(t *testing.T) {
	const url = "https://router.example.invalid"
	const partialTail = " A router client needs all three; router.New builds nothing without " +
		"them, so this server behaves exactly as if no router were configured and will NOT " +
		"report ready while a notes store is wired"
	cases := []struct {
		name        string
		cfg         config
		wantLine    string
		mustContain []string
		mustNotSay  []string
	}{
		{
			name: "all three set: configured",
			cfg:  config{RouterURL: url, RouterToken: "t", RouterActor: "muster"},
			wantLine: `permission router: CONFIGURED — ` + envRouterURL + `=` + url +
				`, actor "muster" — session liveness, event publish, notifications and the ` +
				`directory picker are live (the claude-code approval ` +
				`relay is NOT: see the approval relay gate line)`,
			// It is the only branch that may call itself CONFIGURED.
			mustContain: []string{"CONFIGURED", url},
			// 🔴 "NOT CONFIGURED" AND "PARTIALLY" ARE THE MEASURED HOLE. Without
			// them this case is satisfied by the partial branch, whose sentence
			// contains "CONFIGURED" as a substring and, with every variable set,
			// says nothing "is unset" either.
			mustNotSay: []string{"is unset", "NOT CONFIGURED", "PARTIALLY"},
		},
		{
			// 🔴 THE MEASURED CASE: URL set, token forgotten.
			name: "url set, token missing",
			cfg:  config{RouterURL: url, RouterActor: "muster"},
			wantLine: `permission router: PARTIALLY CONFIGURED, THEREFORE NOT CONFIGURED — ` +
				envRouterURL + ` is set (` + url + `) but ` + envRouterToken + ` is unset.` +
				partialTail,
			mustContain: []string{envRouterToken, "is unset", url},
			// The old banner's exact wrong sentence. The URL is RIGHT here, and
			// telling the operator it is unset is the whole defect.
			mustNotSay: []string{envRouterURL + " is unset"},
		},
		{
			name: "url set, actor missing",
			cfg:  config{RouterURL: url, RouterToken: "t"},
			wantLine: `permission router: PARTIALLY CONFIGURED, THEREFORE NOT CONFIGURED — ` +
				envRouterURL + ` is set (` + url + `) but ` + envRouterActor + ` is unset.` +
				partialTail,
			mustContain: []string{envRouterActor, "is unset"},
			mustNotSay:  []string{envRouterURL + " is unset", envRouterToken + " is unset"},
		},
		{
			name: "url set, both credentials missing",
			cfg:  config{RouterURL: url},
			wantLine: `permission router: PARTIALLY CONFIGURED, THEREFORE NOT CONFIGURED — ` +
				envRouterURL + ` is set (` + url + `) but ` + envRouterToken + ` and ` +
				envRouterActor + ` are unset.` + partialTail,
			mustContain: []string{envRouterToken, envRouterActor, "are unset"},
			mustNotSay:  []string{envRouterURL + " is unset"},
		},
		{
			name: "nothing set, standalone declared",
			cfg:  config{Standalone: true},
			wantLine: `permission router: NONE, DECLARED (` + envStandalone + `=1) — session ` +
				`transcript links will all read "no transcript recorded", which is TRUE here ` +
				`because nothing records transcripts. Notifications and the directory picker ` +
				`degrade and say so where a human can see it`,
			mustContain: []string{envStandalone},
			mustNotSay:  []string{envRouterToken + " is unset", "CONFIGURED"},
		},
		{
			// The genuinely-unset case must keep naming the URL, or fixing the
			// finding above would just move the wrong answer to a different input.
			name: "nothing set at all",
			cfg:  config{},
			wantLine: `permission router: NONE, UNDECLARED — ` + envRouterURL + ` is unset ` +
				`and ` + envStandalone + ` is not 1. This server will NOT report ready while ` +
				`a notes store is wired, on purpose: see /readyz for the reason`,
			mustContain: []string{envRouterURL, "UNDECLARED"},
			mustNotSay:  []string{envRouterToken + " is unset"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := routerLine(t, renderBanner(t, tc.cfg, api.Extensions{}))

			// 🔴 THE WHOLE LINE, NORMALISED ON WHITESPACE ONLY. Folding runs of
			// spaces is what lets the Printf keep its source wrapping; nothing
			// else is folded, so a changed word is a changed line.
			if got, want := normaliseBannerLine(line), normaliseBannerLine(tc.wantLine); got != want {
				t.Errorf("the router banner line is not the sentence this case pins.\n"+
					"  got:  %s\n  want: %s\n"+
					"    This line is READ BY AN OPERATOR MID-INCIDENT and its previous two "+
					"revisions each stated something false — first blaming the wrong "+
					"variable, then calling a fully configured router NOT CONFIGURED. If "+
					"you changed the wording on purpose, update wantLine; if you did not, "+
					"a branch is being selected that this configuration should not reach.",
					got, want)
			}

			for _, want := range tc.mustContain {
				if !strings.Contains(line, want) {
					t.Errorf("the router banner line does not contain %q, so an operator "+
						"cannot act on it.\n  line: %s", want, line)
				}
			}
			for _, forbidden := range tc.mustNotSay {
				if strings.Contains(line, forbidden) {
					t.Errorf("the router banner line SAYS %q, which is false for this "+
						"configuration. A line naming the wrong variable sends the reader "+
						"to re-check the one that was already right — worse than silence.\n"+
						"  line: %s", forbidden, line)
				}
			}
		})
	}
}

// normaliseBannerLine folds runs of whitespace so a pinned line can be written
// across several source lines. It folds NOTHING else — not case, not
// punctuation — because every one of those carries meaning in this banner.
func normaliseBannerLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// bannerLedger is every environment variable logBanner is responsible for
// announcing, in BOTH directions.
//
// 🔴 THIS IS THE GUARD WHOSE ABSENCE LET MUSTER_SECURE_COOKIES BE OMITTED.
// logBanner's header states a RELATIONSHIP — "each line below is printed whether
// the thing is on or off" — and nothing checked it, so a variable could be added
// to config.go with no banner line and every test stayed green. A per-variable
// spot check would have had the same hole for the next variable.
//
// 🔴 AND FOR A WHOLE REVISION THAT LAST SENTENCE READ "this is the relationship,
// asserted", WHICH WAS FALSE. This list is hand-written, so what was asserted
// was "every LEDGERED variable is announced" — not "every variable logBanner is
// responsible for". The omission it exists to prevent therefore still fitted
// through it exactly as MUSTER_SECURE_COOKIES had: measured by adding an
// envNewThing constant to config.go, wiring it once so the spelled-once-AND-READ
// gate was satisfied, and giving it no banner line and no entry here — the full
// suite stayed green.
//
// 🔴 THE CANDIDATE SET IS DERIVED NOW. TestEveryDeclaredEnvVariableIsLedgeredOrExempt
// reads config.go's const block — the same extraction
// TestEveryServerEnvNameIsSpelledOnceAndRead uses — and requires every constant
// in it to be on THIS list or on bannerExempt with a stated reason. A new
// variable is on neither, so it fails until somebody decides which it is.
//
// ⚠ IT IS STILL NOT "EVERY CONSTANT IN config.go", and that honesty is kept
// rather than papered over: see bannerExempt, which now has to NAME each
// exclusion instead of gesturing at a parenthesised handful in prose.
var bannerLedger = []string{
	envDatabase,
	// The Agents list's Idle section: one line in both directions (ON with the
	// threshold / OFF).
	envAgentsIdleAfter,
	envUIPassword,
	// Accounts PR 4: the mode, muster's GoTrue (its secret is named, never
	// printed), and the browser listener — each on its own line in both
	// directions (logAuthBanner).
	envAuthMode,
	envGoTrueURL,
	envGoTrueSecret,
	envBrowserPort,
	envSecureCookies,
	envHookToken,
	envServiceToken,
	envRouterURL,
	// The approval relay's own gate: its off-state is "every claude-code prompt
	// falls back to the terminal", which nothing else on the banner reveals when
	// the router line says NONE by design (a standalone deployment).
	envApprovalGateURL,
	envApprovalGateToken,
	// The cairn memo relay: one line names both, in all three of its states
	// (logMemoRelayBanner). Its off-state is the one a deployment waits in
	// until the credential is issued, so it has to be readable off the log.
	envMemoURL,
	envMemoToken,
	envMemoScopeMap,
	// Dictation: the `dictation:` line names both in both directions (ON / OFF).
	// Its off-state — no microphone on any message box — errors nowhere.
	envSTTURL,
	envSTTToken,
	// File attachments: the `file attachments:` line names all eight in both
	// directions (ON / OFF). Its off-state — no paperclip, every upload route
	// 404 — errors nowhere, and a MISCONFIGURED set is off, not a refusal.
	envS3Endpoint,
	envS3Bucket,
	envS3AccessKey,
	envS3SecretKey,
	envS3Region,
	envUploadSigningKey,
	envUploadMaxBytes,
	envUploadLinkTTL,
	// muster-native Web Push: the `web push:` line names all three in both
	// directions (ENABLED / OFF). Its off-state — no Enable button, nothing
	// sent — is otherwise invisible: nothing errors when push is off.
	envVAPIDPublicKey,
	envVAPIDPrivateKey,
	envVAPIDSubject,
	// The operator push hosts: with push ENABLED the banner names this variable
	// whether it is set (the hosts it adds) or unset (browser hosts only). With
	// push OFF nothing is sent anywhere, so the hosts are moot.
	envPushExtraHosts,
	// The Android pairing policy: a line in both directions (set / unset).
	envAndroidAllowPINKeys,
	envGitHubEncKey,
	envAgentProvisioner,
	// The chat tier is its own axis with its own two lines — the fully-off arm names
	// both variables, the CHAT arm names this one in each direction. It is ledgered
	// rather than exempt because it is precisely a thing this banner is responsible
	// for: the runtime it names decides the bearer derivation, and a wrong one is a
	// 401 from inside a turn.
	envAgentGateway,
	// 🔴 LEDGERED, NOT EXEMPT, AND IT WAS EXEMPT IN A FIRST DRAFT. The port reaches
	// the cluster at CREATE time and is read back from the Deployment's annotation
	// (k8s.AnnotationPort, which is where that fact is stated),
	// not from the live configuration — so a wrong value silently shapes every
	// instance provisioned in that window and correcting the variable afterwards does
	// not move them. The LIFECYCLE arm prints the resolved port; the fully-off arm
	// says the variable is UNREAD because nothing is provisioned. See the retraction
	// in bannerExempt.
	envAgentGatewayPort,
	// 🔴 THE RUNTIME-CONFIG PAIR IS LEDGERED, AND ITS OFF-STATE IS THE LOUDEST ONE
	// ON THIS BANNER. Unset with a runtime named is REFUSED at boot, so the only
	// reachable off-state is "no runtime named" — and in that state a deployment
	// with a kubernetes driver provisions pods that exit 78 and crashloop for ever,
	// which is the measured defect these variables exist to close. A silent version
	// of that state is what shipped: the pod was 0/1, nothing timed it out, and the
	// cause was only in `kubectl logs`. The CHAT arms report the installed state in
	// both directions and the fully-off arm names the pair as inert.
	//
	// ⚠ TWO ENTRIES AND NOT ONE, even though they are a both-or-neither pair. The
	// ledger's unit is the DECLARED CONSTANT — TestEveryDeclaredEnvVariableIsLedgeredOrExempt
	// walks config.go's const block — so one entry would leave the other on neither
	// list, which is the gap that test exists to close.
	envAgentRuntimeConfig,
	envAgentRuntimeInstall,
	// 🔴 THE CONNECTOR STORE'S THREE ARE LEDGERED: the key is the feature's on/off
	// switch, and the port and the agent URL decide what an isolated agent can
	// reach — a silent state for any of them is "the proxy is not where agents were
	// told it is" with nothing in the log. One line names all three in each
	// direction.
	envConnectorEncKey,
	// The GitHub App: the `github app:` line names all three in both directions
	// (ON / OFF), and the `checkout wait:` line names its variable in both.
	envGitHubAppID,
	envGitHubAppKey,
	envGitHubAppAccounts,
	envCheckoutWaitTimeout,
	envConnectPort,
	envAgentConnectURL,
	// 🔴 THE MIRROR PLUGIN IS LEDGERED: ON, it accepts host connections on a route
	// that answers 404 when OFF, and an operator installing muster-mirror needs the
	// log to say which of the two this deployment is before blaming the host.
	envMirror,
	// 🔴 THE PLUGIN INTERFACE IS LEDGERED for the mirror's reason: OFF, every
	// /plugin/* route answers 404, and a plugin author needs the log to say which
	// state this deployment is in (and, ON without a gateway, that nothing is
	// delivered).
	envPlugins,
	// The plugin task namespaces: unset, POST /plugin/v1/tasks answers 503 and a
	// plugin that files tasks holds its notices, so the operator must be told
	// which state this is (and which namespaces are accepted).
	envPluginTaskNamespaces,
	// 🔴 SESSION ROUTING IS LEDGERED: ON, the chief can put an outside party's
	// message into the operator's own sessions and relay the answer without
	// approval to allowlisted senders, so the log must say which state this is
	// and how many owners have an allowlist.
	envSessionRouting,
	envRouteRelayAllow,
	// The route deadline: ON, the line names the value a route waits before it
	// goes back to the chief; OFF, that it is not read.
	envRouteTimeout,
	// 🔴 TASK ROUTING IS LEDGERED: ON, an outside system's task notice reaches
	// the chief, and with dispatch the chief can start agents on the owner's
	// credentials, so the log must say which state this is.
	envTaskRouting,
	// CHIEF SESSION RUNTIMES ARE LEDGERED: ON, the chief's sessions may run on
	// another runtime and model than its own, so the log must name which.
	envChiefSessionRuntimes,
	// 🔴 THE AGENT-FLOW DENY WORDS ARE LEDGERED: unset, the agent door screens
	// agent-composed text with the generic patterns only, and that fail-open
	// state must be a loud line, not a silence (the operator's audit of #76).
	envFlowAgentDenyWords,
}

// bannerExempt is every environment variable config.go declares that logBanner
// is deliberately NOT responsible for announcing in both directions, and why.
//
// 🔴 A REASON IS REQUIRED, AND THE SPLIT IS MACHINE-CHECKED RATHER THAN TRUSTED.
// An exception list is only better than no guard if it cannot be used as a
// silencer, so TestEveryDeclaredEnvVariableIsLedgeredOrExempt refuses an entry
// here whose variable IS named in the banner in both directions: that is the
// definition of a ledgered variable, and the entry would be hiding a line the
// ledger's both-directions and lines-must-differ checks would otherwise hold to
// account. Adding a banner line for one of these is therefore not a silent
// change — it fails here until the variable is moved onto bannerLedger.
//
// ⚠ EACH REASON BELOW WAS READ OFF THE CODE, NOT INFERRED FROM THE GROUPING.
// The prose this replaced claimed MUSTER_PUBLIC_URL and GITHUB_CLIENT_SECRET
// were "reported through a different name"; githubAuthPosture names
// GITHUB_CLIENT_ID and MUSTER_GITHUB_TOKEN and neither of the other two, so the
// entries say what is actually true of them.
var bannerExempt = map[string]string{
	envChiefDispatchOwners: "counted on the `task routing: ON with DISPATCH` line (with the caps), named on the ON line " +
		"when empty, and on a `chief dispatch: INERT` line when set with task routing off",
	envStepUpWindow: "printed with its VALUE on the `mirrored sessions: ENABLED` line, the one plugin whose " +
		"writes it gates; it has no OFF state (unset is the 12h default, an out-of-bounds value refuses to " +
		"boot in loadConfig), and with the plugin off nothing reads it",
	envAgentKinds: "announced on the `agent kinds:` line in both arms (gateway-only and claude-code " +
		"enabled); the claude-code arm cannot be rendered by bannerBothDirections' fixture, whose " +
		"provisioner is built without an account pool. TestTheKindsBannerLineNamesAccountsNeverTokens " +
		"renders both arms and pins them",
	envAgentCCImage:       "printed on the claude-code arm of the `agent kinds:` line; refused at boot when the kind is not enabled (validateKinds), so there is no OFF state to announce",
	envAgentCCAccounts:    "printed on the claude-code arm of the `agent kinds:` line (names only); same reason as " + envAgentCCImage,
	envAgentCCTokenPrefix: "a PREFIX naming subscription credentials: the banner names the prefix and never a value (TestTheKindsBannerLineNamesAccountsNeverTokens)",
	envAgentCCStorage:     "an optional size for the claude-code volume; refused at boot without the kind, defaulted with it (agentspec.DefaultClaudeCodeStorage)",
	envAgentCCCooldown: "an optional limit of the claude-code account pool; refused at boot without the kind, defaulted " +
		"with it (ccpool.DefaultCooldown, config.ccPoolLimits), so there is no OFF state to announce",
	envAgentCCMaxJobs: "an optional, opt-in limit of the claude-code account pool (unset is no cap, config.ccPoolLimits); " +
		"refused at boot without the kind, so there is no OFF state to announce",
	envAgentCCBypassPools: "printed on the claude-code arm of the `agent kinds:` line in both states (the pools listed, or " +
		"\"no worker pool bypasses\"), which bannerBothDirections' fixture cannot render; refused at boot without the " +
		"kind (validateClaudeCodeKind), so outside that arm there is no state to announce. " +
		"TestTheKindsBannerLineAnnouncesTheJobLaneBypassBothWays pins both",
	envAgentNetpolFromNS: "named on the `claude-code network:` line in both arms (a policy is written / NOT CONFINED), " +
		"which bannerBothDirections' fixture cannot render for the reason given for " + envAgentKinds + "; " +
		"TestTheNetworkBannerLineSaysWhetherAgentsAreConfined renders both and pins them. There is no silent " +
		"state: validateKinds refuses it unset when a policy would be written and set when none would be",
	envAgentNetpolFromLabels: "the other half of the same selector; same line, same test and same boot refusals as " + envAgentNetpolFromNS,
	envE2EHoldReadyDir: "a browser-test fixture read ONLY by a `-tags e2eterm` build's fake driver (e2eterm.go); the " +
		"shipped binary has no reader, so it has no state to announce, and an e2e build already logs its own " +
		"🔴 E2E BUILD line at boot",
	envE2ETermWindow: "the same: a browser-test fixture read ONLY by a `-tags e2eterm` build's fake driver " +
		"(the session window its fake pod reports); the shipped binary has no reader",
	envE2EGitHubAPI: "the same: a browser-test fixture read ONLY by a `-tags e2eterm` build (e2eupstreams.go), " +
		"pointing GitHub calls at the pickers spec's fake; the shipped binary has no reader",
	envE2EOpenRouterModels: "the same: a browser-test fixture read ONLY by a `-tags e2eterm` build (e2eupstreams.go), " +
		"pointing the model search at the pickers spec's fake catalogue; the shipped binary has no reader",
	envE2EAgentEndpoint: "the same: a browser-test fixture read ONLY by a `-tags e2eterm` build's fake driver " +
		"(every agent endpoint at the answer-delivery spec's loopback fake runtime); the shipped binary has no reader",
	envE2EGoTrueSkew: "the same: a browser-test fixture read ONLY by a `-tags e2eterm` build (e2egotrue.go), " +
		"shortening the GoTrue verifier's clock skew for the specs that outlive an access token; the shipped binary has no reader",
	envAgentGatewayModel: "the CHAT: WIRED line prints it beside the scheme, and there is no " +
		"off-direction line to write: validate refuses a named gateway with no sentinel, so the " +
		"only states that reach a banner are \"wired, and the value is on the line\" and \"no " +
		"gateway at all\", which the CHAT: UNWIRED line already reports by naming " +
		"MUSTER_AGENT_GATEWAY. There is no silent state for a second line to disambiguate — the " +
		"boot refusal is what covers the missing-value case, and it names the variable",
	envPort: "the listen line reports the BOUND ADDRESS (main.go: \"muster %s listening on %s\" " +
		"with ln.Addr()), which is the answer this variable is asked for and is strictly " +
		"better than echoing the request — it is printed after the banner, by serve, not by " +
		"logBanner",
	envGitHubToken: "named by githubAuthPosture on the github-connection line whenever a " +
		"credential exists; there is no off-line for it because the no-credential branch " +
		"names it too, but only when MUSTER_GITHUB_ENCRYPTION_KEY is set at all — the " +
		"encryption key is the ledgered variable that gates the whole surface",
	envGitHubClientID: "named by githubAuthPosture as \"the OAuth App\" on the " +
		"github-connection line; same one-directional shape as MUSTER_GITHUB_TOKEN and for " +
		"the same reason",
	envGitHubClientSec: "the OAuth App is reported by its CLIENT ID, which is the half that " +
		"is safe to print and the half an operator checks. A secret's NAME in a log tells " +
		"the reader nothing its sibling has not already told them",
	envPublicURL: "the OAuth callback base URL — an input to the same GitHubOAuthConfig as " +
		"the client id and secret (main.go), and reported through the same posture line",
	envFaroURL: "browser RUM: handed to ui.SetFaroConfig for the page agent, not a property " +
		"of this server's auth or dependency posture. logBanner has no line for it on purpose",
	envFaroKey: "browser RUM: the collect-endpoint key, handed to ui.SetFaroConfig with the " +
		"URL and reaching the page rather than this server. Same reason as MUSTER_FARO_URL, " +
		"and it is client-public by design so its absence breaks no server posture",
	envFaroTracing: "browser RUM: a tracing toggle on the page agent, handed to " +
		"ui.SetFaroConfig with the URL and the key. It changes what the BROWSER emits, and " +
		"nothing about what this process will or will not do at boot",
	envHostOrder: "display only: the order of the Agents list's host tabs, handed to " +
		"ui.SetHostPreferences. No server posture changes with it, and a value it refuses " +
		"stops the boot in loadConfig, so there is no silent refusing state to announce",
	envHostIcons: "display only: a host's icon by its name, handed to ui.SetHostPreferences " +
		"with MUSTER_HOST_ORDER. Same reason as MUSTER_HOST_ORDER",
	envTagAutoDispatch: "a feature flag on the task surface (api.Extensions.TagAutoDispatch), " +
		"not a refusal or a dependency. Nothing about the boot posture changes with it, so " +
		"a banner line would be noise",
	envSendQueueIdle: "the send queue's dispatcher announces its own state with the VALUE when it " +
		"starts (internal/api/send_queue.go: \"sendqueue: dispatcher started: …idle for %s…\"); " +
		"with no database there is no queue and nothing reads it, and an out-of-bounds value refuses to boot",
	envFocusRetention: "the focus refresher's pruning logs each prune with the VALUE (internal/focus: \"pruned %d " +
		"mirrored session(s) not seen for %s\"); an out-of-bounds value refuses to boot",
	envFocusInterval: "the focus refresher announces its own state with the VALUE when it starts " +
		"(internal/api/focus.go: \"focus: refresh every %s …\"); with no database or no " +
		"summarizer nothing reads it, and an out-of-bounds value refuses to boot",
	envTaskReapAfter: "the reaper announces its own state with the VALUE, at the point it " +
		"starts (internal/api/server.go: \"task reaper: sweeping every %s for tasks idle for " +
		">%s\"), which is where a reader looking for it will be",
	envAgentReapStoppedAfter: "the agent reaper announces its own state with the VALUE, or that it " +
		"is off, at the point it starts (internal/api/server.go: \"agent reaper: sweeping every %s " +
		"for agents stopped for >%s\" / \"agent reaper: off\"); a value it refuses stops the boot",
	envRouterToken: "reported by the permission-router line, which names whichever credential " +
		"is MISSING (missingRouterCredential). A fully configured deployment deliberately " +
		"says nothing about it, so it cannot satisfy the both-directions rule — " +
		"TestTheRouterBannerNamesTheVariableThatIsActuallyMissing is what holds it",
	envRouterActor: "reported by the permission-router line; see MUSTER_ROUTER_TOKEN",
	envStandalone: "reported by the permission-router line's two NONE branches (DECLARED and " +
		"UNDECLARED). When a router IS configured the line has nothing to say about it, " +
		"which is the one-directional shape the ledger cannot express",

	// --- agent provisioning ---------------------------------------------------
	//
	// 🔴 MUSTER_AGENT_PROVISIONER IS LEDGERED, NOT EXEMPT, AND EVERYTHING BELOW IT
	// IS A PARAMETER OF THE BACKEND THAT KNOB SELECTS. The banner reports TIERS:
	// whether a dependency is wired and which door refuses when it is not. A
	// dozen driver and spec parameters on the banner would drown the four lines an
	// operator actually reads, and each reason below names what reports the
	// variable INSTEAD — either a boot refusal (the process does not start, so the
	// absence cannot be silent) or a documented default.
	//
	// ⚠ EACH REASON WAS READ OFF THE CODE IT CITES, and one candidate reason was
	// discarded for being false: "the noop driver has no storage to persist to"
	// was measured against provision.DefaultNoopCapabilities, which declares
	// Persistence TRUE.
	envAgentImageRepo: "config.validateProvisioner REFUSES to start when a provisioner is " +
		"named and this is unset, so its absence is a fatal line at boot rather than a " +
		"silent state a banner would have to announce. agentspec.Build refuses it too, but " +
		"that refusal happens inside a dispatch goroutine",
	envAgentAPIURL: "same boot refusal as MUSTER_AGENT_IMAGE_REPO — an instance with no base " +
		"URL cannot reach this server, and validateProvisioner names that consequence",
	envAgentImageTag: "defaults to agentspec.DefaultImageTag (\"latest\"), which is a property " +
		"of the spec an instance is built from rather than of this server's boot posture",
	// 🔴 MUSTER_AGENT_GATEWAY_PORT WAS EXEMPTED HERE AND IT WAS WRONG — IT IS ON
	// bannerLedger NOW. The reason read: "There is no off-direction line to write:
	// with no gateway named, nothing in this process resolves an endpoint at all, so
	// the CHAT: UNWIRED line has nothing to say about the port." That is a true
	// statement about RESOLUTION and the hazard is at CREATE time:
	// agentspec.Build declares the port unconditionally, so with a provisioner wired
	// it shapes every instance's Service whether chat is on or not, and
	// Driver.Endpoint reads the create-time annotation rather than the current
	// configuration — so a wrong value is not fixed by correcting it later. The
	// MUSTER_AGENT_GATEWAY_MODEL exemption below is a genuinely different shape:
	// validate REFUSES a named gateway with no sentinel, so that variable has no
	// silent state. This one did, and an entry here describing a silent state is
	// exactly the silencer this map's header forbids.
	envAgentModel: "empty means the RUNTIME decides (agentspec.Config.Model), which is why " +
		"Build omits the config key entirely rather than writing \"\". It reaches the " +
		"instance, not this process",
	envAgentOpenRouterKey: "a model-provider credential that is injected into the INSTANCE's " +
		"secrets (agentspec.EnvOpenRouterKey); empty means the installation expects the " +
		"runtime image to carry its own, per agentspec.Config's own doc. ⚠ WORTH " +
		"REVISITING WHEN api.Gateway IS WIRED: at that point a missing provider credential " +
		"becomes a failure of THIS server's chat path rather than of the instance",
	envAgentNamespace: "the shared-layout namespace. The combination that could be silently " +
		"wrong — shared layout with no namespace — is refused by validateProvisioner at " +
		"boot, and TestTheSharedNamespaceLayoutIsRefusedWithoutANamespace holds that",
	envAgentNSPrefix: "the per-instance namespace prefix, whose one silent hazard is " +
		"disagreeing with what internal/api writes into agents.namespace. That is pinned " +
		"structurally by TestTheStoredNamespacePrefixIsWhatTheDriverIsConfiguredWith, " +
		"which is a stronger check than a log line a reader has to compare by eye",
	envAgentNSShared: "selects between the two namespace layouts; see " +
		"MUSTER_AGENT_NAMESPACE and MUSTER_AGENT_NAMESPACE_PREFIX, both of which are " +
		"guarded rather than announced",
	envAgentWorkspaceKeep: "asks the driver for a workspace that survives a restart. Unset " +
		"means ephemeral, and a driver that cannot persist refuses the spec with " +
		"provision.ErrUnsupported at dispatch, naming itself — see " +
		"k8s.Config.WorkspaceStorageClass's three states",
	envAgentStorageClass: "only read when MUSTER_AGENT_WORKSPACE_PERSIST is set, where empty " +
		"means the cluster's default StorageClass. A banner line for it would report a " +
		"value that is ignored in the default configuration",
	envAgentEndpointTmpl: "overrides the driver's own DefaultEndpointTemplate. It decides " +
		"where an instance is REACHED, which the driver resolves per instance " +
		"(provision.Provisioner.Endpoint) rather than once at boot",
	envAgentCairnURL: "one half of a BOTH-OR-NEITHER pair reaching agentspec.Config. Unset " +
		"is the shipping state and in it NOTHING is emitted — no install step, no " +
		"credential file, no environment variable — so there is no posture for a banner " +
		"line to report. The half that could be silently wrong is a URL with no token " +
		"(or the reverse), and that is not announced either: it is gated by " +
		"agentspec.Config.CairnConfigured, which both the spec and the supervisor's " +
		"prose read, and pinned by " +
		"TestTheSupervisorsProseAndItsSpecAgreeAboutTheStore",
	envAgentCairnToken: "the other half of the pair; see MUSTER_AGENT_CAIRN_URL. ⚠ IT IS A " +
		"READ+WRITE CREDENTIAL across every scope of the operator's knowledge store, " +
		"which is the positive reason it is NOT on the banner: a boot line naming it " +
		"would put a line about a live write key into every log this process emits, and " +
		"the only fact a reader needs — whether the integration is on — is visible from " +
		"the instance's own spec",
}

// bannerStubGitHubStore stands in for a wired GitHub store. Only its presence
// is read by logBanner (`ext.GitHub != nil`), never any method, so embedding the
// interface is enough and cannot accidentally be CALLED without panicking
// loudly — which is the behaviour wanted if the banner ever starts reading it.
type bannerStubGitHubStore struct{ github.Store }

// bannerStubConnectorStore stands in for a wired connector store; only its
// presence is read.
type bannerStubConnectorStore struct{ connectors.Store }

// bannerProvisioner builds the REAL lifecycle adapter over the noop driver, for
// the banner's wired arm.
//
// ⚠ IT IS NOT A STUB, AND THE REASON IS THE ONE bannerStubGitHubStore GIVES IN
// REVERSE. logBanner reads only `ext.Provisioner != nil`, so a stub would do — but
// this one is free, exercises buildAgentPlane (the function whose result the
// banner is describing), and cannot drift from what the binary actually assigns.
func bannerProvisioner(t *testing.T, cfg config) api.Provisioner {
	t.Helper()
	p, _, err := buildAgentPlane(cfg, stubStore{}, log.New(&strings.Builder{}, "", 0))
	if err != nil {
		t.Fatalf("building the banner's provisioner fixture: %v", err)
	}
	if p == nil {
		t.Fatalf("the banner fixture's config (%s=%s) produced no provisioner, so the "+
			"WIRED arm would never be rendered", envAgentProvisioner, cfg.AgentProvisioner)
	}
	return p
}

// bannerGateway is the chat half of the same fixture, built through the real
// wiring for the same reason bannerProvisioner is: a stub would let the banner's
// WIRED arm render over a gateway the binary cannot actually construct.
func bannerGateway(t *testing.T, cfg config) api.Gateway {
	t.Helper()
	_, gw, err := buildAgentPlane(cfg, stubStore{}, log.New(&strings.Builder{}, "", 0))
	if err != nil {
		t.Fatalf("building the banner's gateway fixture: %v", err)
	}
	if gw == nil {
		t.Fatalf("the banner fixture's config (%s=%s) produced no gateway, so the CHAT: WIRED "+
			"arm would never be rendered", envAgentGateway, cfg.AgentGateway)
	}
	return gw
}

// TestTheBannerAnnouncesEveryLedgeredVariableInBothDirections is the F6 guard.
// bannerBothDirections renders the banner twice from configurations that are
// deliberately opposites on every ledgered axis.
//
// 🔴 ONE FIXTURE, TWO READERS. The ledger test below and the exemption test
// after it both need "is this variable named when it is on / when it is off",
// and two copies of the fixture is how the two tests come to disagree about
// what ON means — at which point a variable can be ledgered against one
// definition and exempted against another, and the split they jointly enforce
// stops meaning anything. A single "fully on" fixture would also pass for a
// variable whose off-branch prints nothing, which is exactly the defect.
func bannerBothDirections(t *testing.T) (onOut, offOut string) {
	t.Helper()
	on := config{
		Database:            "postgres://x/y",
		UIPassword:          strings.Repeat("p", 40),
		SecureCookies:       true,
		AndroidAllowPINKeys: true,
		HookToken:           "hook",
		ServiceToken:        strings.Repeat("s", 40),
		AuthMode:            string(api.AuthModeGoTrue),
		GoTrueURL:           "http://auth.example.invalid:9999",
		GoTrueSecret:        strings.Repeat("j", 40),
		BrowserPort:         18192,
		RouterURL:           "https://router.example.invalid",
		RouterToken:         "t",
		RouterActor:         "muster",
		// The relay's dedicated gate, so the ON render takes the DEDICATED arm.
		ApprovalGateURL:   "https://gate.example.invalid",
		ApprovalGateToken: "g",

		// 🔴 THE PROVISIONER AXIS NEEDS BOTH THE CONFIG *AND* THE WIRED EXTENSION,
		// because the banner branches on the extension and reports the config. A
		// fixture that set only the variable would take the UNWIRED arm and the two
		// renders would differ in nothing but a word inside one line — which the
		// lines-must-differ check would pass while the WIRED arm stayed unrendered
		// by any test. That is the same shape as the *pgxpool.Pool note above.
		AgentProvisioner: provisionerNoop,
		AgentImageRepo:   "registry.example.test/muster/agent-runtime",
		AgentAPIURL:      "http://muster.example.test:8105",

		// 🔴 THE CHAT AXIS IS SEPARATE AND NEEDS ITS OWN FLIP, FOR EXACTLY THE REASON
		// THE NOTE ABOVE GIVES ABOUT THE PROVISIONER. The two tiers have two variables
		// and two banner lines: a fixture that named a runtime without wiring
		// ext.Gateway would render the CHAT: UNWIRED arm, and the WIRED arm — the one
		// that names which bearer derivation this process uses — would be unrendered by
		// any test while both renders still differed elsewhere.
		AgentGateway:      gatewayHooksSHA256,
		AgentGatewayModel: "runtime-sentinel",
		// 🔴 AND NAMING A RUNTIME NOW REQUIRES THE RUNTIME-CONFIG BUNDLE, so the
		// fixture carries it for the same reason it carries the sentinel: without it
		// this is not a configuration the binary would boot on, and the CHAT: WIRED
		// arm — the only place the bundle's state is reported — would be rendered
		// from an invalid config. bannerGateway runs the real buildGateway over this
		// value, so an incomplete fixture is not merely unrealistic.
		AgentRuntimeConfig:  fixtureRuntimeConfigTemplate,
		AgentRuntimeInstall: fixtureRuntimeInstallScript,

		// The connector store: the key, the dedicated listener and the URL agents
		// are told. The ON render also needs ext.Connectors (below), because the
		// banner branches on the extension, as for GitHub.
		ConnectorEncKey: make([]byte, 32),
		ConnectPort:     18191,
		AgentConnectURL: "http://muster.example.test:18191",

		// The mirror plugin: the banner branches on ext.Mirror, as for connectors.
		Mirror: true,

		FlowAgentDenyWords: []string{"examplecluster"},
		// The memo relay: ON needs the credential, a pool and ext.Gateway (both
		// supplied below), which is memoRelayEnabled's whole predicate.
		MemoURL:   "http://memos.example.invalid:8125",
		MemoToken: "memo-credential-fixture",
		// A synthetic split: one repository whose memos are filed under two names.
		MemoScopeMap: "widgets=widgets,widgets-clone",
		// Plugin tasks: the banner branches on the list (and on ext.Plugins, below).
		PluginTaskNamespaces: []string{"tracker"},
		// Dictation: the banner branches on ext.Transcriber (below).
		STTURL:   "http://stt.example.invalid:8000",
		STTToken: "stt-credential-fixture",
		// File attachments: the banner branches on ext.Uploads (below), built by
		// the real buildUploads over these values and the pool.
		PublicURL:        "https://muster.example.test",
		S3Endpoint:       "http://minio.example.invalid:9000",
		S3Bucket:         "muster-uploads",
		S3AccessKey:      "s3-access-fixture",
		S3SecretKey:      "s3-secret-fixture",
		S3Region:         "us-east-1",
		UploadSigningKey: strings.Repeat("u", 40),
		UploadMaxBytes:   "1048576",
		UploadLinkTTL:    "2h",
		// Web Push: the banner branches on ext.Push (below), as for connectors.
		VAPIDPublicKey:  bannerVAPIDPub,
		VAPIDPrivateKey: bannerVAPIDPriv,
		VAPIDSubject:    "mailto:ops@example.com",
		// Session routing: the banner branches on ext.Routing (below).
		SessionRouting:     true,
		RouteRelayAllowRaw: "1:example-inbox:someone@example.invalid",
		// Task routing: the banner branches on ext.TaskRouting (below).
		TaskRouting:            true,
		ChiefDispatchOwnersRaw: "1",
		ChiefDispatchOwners:    map[int64]bool{1: true},
		// Chief session runtimes: the banner branches on ext.SessionRuntimes (below).
		ChiefSessionRuntimes: []string{"claude-code", "gateway"},
		// The GitHub App: the banner branches on ext.GitHubApp (below); the key
		// itself is never printed, so the fixture needs none.
		GitHubAppID:         12345,
		GitHubAppAccounts:   []githubapp.Account{{Login: "example-org", ID: 4200}},
		CheckoutWaitTimeout: 48 * time.Hour,
		// The Agents list's Idle section, at a value that is not the default.
		AgentsIdleAfter:    72 * time.Hour,
		AgentsIdleAfterRaw: "3d",
	}
	onOut = renderBannerWithPool(t, on, api.Extensions{
		GitHubApp:   bannerStubGitHubApp{},
		Mirror:      mirror.NewService(nil, nil),
		Push:        push.NewService(nil, nil, on.vapid(), nil),
		Connectors:  bannerStubConnectorStore{},
		GitHub:      bannerStubGitHubStore{},
		Provisioner: bannerProvisioner(t, on),
		Gateway:     bannerGateway(t, on),
		Transcriber: bannerTranscriber(t, on),
		Uploads:     bannerUploads(t, on),
		// Plugins: the banner branches on ext.Plugins and ext.PluginGateway.
		Plugins:         bannerStubPlugins{},
		PluginGateway:   bannerStubPluginGateway{},
		Routes:          bannerStubRoutes{},
		Routing:         &api.Routing{Allow: bannerAllowlist(t), Timeout: 45 * time.Minute},
		TaskRouting:     &api.TaskRouting{DispatchOwners: map[int64]bool{1: true}},
		SessionRuntimes: []string{"claude-code", "gateway"},
	}, &pgxpool.Pool{})
	offOut = renderBannerWithPool(t, config{}, api.Extensions{}, nil)
	return onOut, offOut
}

func bannerAllowlist(t *testing.T) plugins.RelayAllowlist {
	t.Helper()
	l, err := plugins.ParseRelayAllowlist("1:example-inbox:someone@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// 🟢 Routing ON with no MUSTER_HOOK_TOKEN: a session answering on the shared
// hook credential is refused (503), so its routes time out — a loud line.
// With the token set (the control), no warning.
func TestTheBannerWarnsWhenRoutingIsOnWithoutAHookToken(t *testing.T) {
	ext := api.Extensions{Plugins: bannerStubPlugins{}, PluginGateway: bannerStubPluginGateway{},
		Routes: bannerStubRoutes{}, Routing: &api.Routing{Allow: bannerAllowlist(t)}}
	const warn = "WARNING — session routing: " + envHookToken + " is unset"
	if out := renderBanner(t, config{SessionRouting: true, Plugins: true}, ext); !strings.Contains(out, warn) {
		t.Errorf("routing on, no hook token: no warning.\nbanner:\n%s", out)
	}
	if out := renderBanner(t, config{SessionRouting: true, Plugins: true, HookToken: "hook"}, ext); strings.Contains(out, warn) {
		t.Errorf("routing on WITH a hook token still warns.\nbanner:\n%s", out)
	}
}

// bannerStubPlugins and bannerStubPluginGateway are present-but-inert: the
// banner only asks whether they are wired.
type bannerStubPlugins struct{ plugins.Store }
type bannerStubGitHubApp struct{ api.GitHubApp }
type bannerStubPluginGateway struct{ plugins.Gateway }
type bannerStubRoutes struct{ plugins.RouteStore }

func TestTheBannerAnnouncesEveryLedgeredVariableInBothDirections(t *testing.T) {
	onOut, offOut := bannerBothDirections(t)

	// 🔴 POSITIVE CONTROL: THE TWO RENDERS MUST DIFFER. If the fixture failed to
	// flip anything — a field renamed, a zero value that is also the "on" value —
	// both strings would be identical and every assertion below would be about
	// one configuration twice.
	if onOut == offOut {
		t.Fatal("positive control FAILED: the ON and OFF banners are byte-identical, so " +
			"the fixture is not exercising both directions and a one-direction variable " +
			"would pass unnoticed")
	}

	for _, name := range bannerLedger {
		t.Run(name, func(t *testing.T) {
			onLine := lineNaming(onOut, name)
			offLine := lineNaming(offOut, name)

			if onLine == "" {
				t.Errorf("%s is not named in the banner when it IS set.\nbanner:\n%s", name, onOut)
			}
			if offLine == "" {
				t.Errorf("%s is not named in the banner when it is NOT set.\n"+
					"    logBanner's header promises every line is printed in both "+
					"directions, because a silent healthy state and a silent refusing "+
					"state are byte-identical in the log. %s defaults to off, so the "+
					"OFF line is the only one an operator will ever see.\nbanner:\n%s",
					name, name, offOut)
			}

			// 🔴 THE TWO LINES MUST ALSO DIFFER, AND THIS HALF WAS ADDED BECAUSE
			// THE MUTATION SWEEP WALKED THE FIRST ONE. A mutant that forced the
			// secure-cookie branch never to be taken (`if a.cfg.SecureCookies &&
			// false`) SURVIVED: the else arm still printed the variable's name, so
			// "named in both directions" held while the banner reported the wrong
			// state in one of them. Naming a variable is not reporting it — the
			// whole point is that an operator can read the STATE off the log, and
			// two identical lines carry no state at all.
			if onLine != "" && offLine != "" && onLine == offLine {
				t.Errorf("%s produces the SAME banner line whether it is set or not:\n"+
					"    %s\n"+
					"    The name is there, so a contains-check passes, but the line "+
					"reports no state: an operator reading the log cannot tell which "+
					"way this deployment is configured, which is the exact "+
					"indistinguishability logBanner's header exists to remove. The "+
					"branch is either dead or it prints the same sentence on both "+
					"arms.", name, onLine)
			}
		})
	}
}

// TestEveryDeclaredEnvVariableIsLedgeredOrExempt is what makes bannerLedger's
// header sentence true.
//
// 🔴 THE LEDGER IS A CLAIM ABOUT A SET IT DOES NOT DEFINE. Its own comment said
// "this is the relationship, asserted", and what was asserted was the
// relationship restricted to the list itself — every ledgered variable is
// announced, which says nothing at all about a variable nobody ledgered. That is
// the identical shape as MUSTER_SECURE_COOKIES' omission, one level up: the
// thing that would have caught it was never the spot check, it was somebody
// noticing. Measured: an envNewThing constant added to config.go and referenced
// once — enough for TestEveryServerEnvNameIsSpelledOnceAndRead's "AND READ"
// direction — with no banner line and no ledger entry left the full suite green.
//
// 🔴 SO THE CANDIDATES COME FROM THE CONST BLOCK, VIA THE SAME EXTRACTION THE
// SPELLED-ONCE GATE USES. envConstDecl is shared deliberately: two regexes for
// one declaration shape is two things to keep in step, and a variable invisible
// to one of them would be invisible to whichever gate owned it.
func TestEveryDeclaredEnvVariableIsLedgeredOrExempt(t *testing.T) {
	declared := declaredEnvConstants(t)

	// 🔴 POSITIVE CONTROL, AS A NUMBER. An empty extraction satisfies "every
	// declared variable is ledgered or exempt" perfectly, and reads identically
	// to a clean tree. Same threshold and same MUSTER_PORT probe as the
	// spelled-once gate, for the same reason.
	if len(declared) < 15 {
		t.Fatalf("positive control FAILED: only %d env constant(s) were extracted from "+
			"config.go's const block, and this binary declares ~20. The declaration "+
			"pattern has stopped matching, so the verdict below is over a truncated set.",
			len(declared))
	}
	if _, ok := declared[envPort]; !ok {
		t.Fatalf("positive control FAILED: %s is declared in config.go and the extraction "+
			"did not find it (%d found). The instrument is not measuring.",
			envPort, len(declared))
	}

	ledgered := map[string]bool{}
	for _, name := range bannerLedger {
		ledgered[name] = true
	}

	// --- every declared variable is one or the other --------------------------
	for name, ident := range declared {
		_, exempt := bannerExempt[name]
		switch {
		case ledgered[name] && exempt:
			t.Errorf("%s (%s) is on BOTH bannerLedger and bannerExempt. They are the two "+
				"answers to one question — does logBanner announce this in both "+
				"directions — and a variable on both makes the exemption unreadable: a "+
				"later reader cannot tell which one is the live decision.", name, ident)
		case !ledgered[name] && !exempt:
			t.Errorf("%s (%s) is declared in config.go and is on NEITHER bannerLedger nor "+
				"bannerExempt.\n"+
				"    logBanner's header promises every thing it is responsible for gets a "+
				"line whether it is on or off, because a silent healthy state and a silent "+
				"refusing state are byte-identical in the log. MUSTER_SECURE_COOKIES was "+
				"omitted exactly this way and shipped a session cookie with no Secure "+
				"attribute, unannounced.\n"+
				"    Decide, do not delete this test: give it a banner line in BOTH "+
				"directions and add it to bannerLedger, or add it to bannerExempt with the "+
				"reason it is not this banner's business.", name, ident)
		}
	}

	// --- neither list may name a variable that is not declared ---------------
	for _, name := range bannerLedger {
		if _, ok := declared[name]; !ok {
			t.Errorf("bannerLedger names %q, which config.go's const block does not declare. "+
				"A ledger entry for a variable that no longer exists is a line of coverage "+
				"that can never fail.", name)
		}
	}
	for name, reason := range bannerExempt {
		if _, ok := declared[name]; !ok {
			t.Errorf("bannerExempt names %q, which config.go's const block does not declare. "+
				"A stale exemption silences nothing and misleads the next reader about what "+
				"the banner covers.", name)
		}
		// 🔴 THE REASON IS THE ENTIRE VALUE OF AN EXCEPTION LIST. An empty or
		// token one turns it into an allowlist anybody can append to, which is
		// the mechanism this test exists to remove rather than relocate.
		if len(strings.TrimSpace(reason)) < 40 {
			t.Errorf("bannerExempt[%q] has no real reason (%q). Say what reports this "+
				"variable's state instead, or that nothing does and why that is correct.",
				name, reason)
		}
	}

	// --- an exemption may not hide a variable the banner DOES report ---------
	//
	// 🔴 THIS IS WHAT STOPS bannerExempt BEING A SILENCER. Named in both
	// directions is the definition of a ledgered variable; an entry here for one
	// of those would move it out from under the both-directions and
	// lines-must-differ checks while looking like documentation.
	onOut, offOut := bannerBothDirections(t)
	if onOut == offOut {
		t.Fatal("positive control FAILED: the ON and OFF banners are byte-identical, so " +
			"the both-directions reads below are one configuration twice")
	}
	for name := range bannerExempt {
		if lineNaming(onOut, name) != "" && lineNaming(offOut, name) != "" {
			t.Errorf("%s is on bannerExempt, but the banner names it in BOTH directions:\n"+
				"    on:  %s\n    off: %s\n"+
				"    That is precisely what a LEDGERED variable is. Move it to "+
				"bannerLedger, where the two lines are also required to DIFFER — an "+
				"exemption would leave it announced but unchecked, which reads as coverage "+
				"and provides none.", name, lineNaming(onOut, name), lineNaming(offOut, name))
		}
	}

	t.Logf("%d declared env constant(s): %d ledgered, %d exempt",
		len(declared), len(bannerLedger), len(bannerExempt))
}

// declaredEnvConstants reads config.go's const block and returns env name ->
// identifier, using the SAME declaration pattern as the spelled-once gate.
func declaredEnvConstants(t *testing.T) map[string]string {
	t.Helper()
	body, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		if m := envConstDecl.FindStringSubmatch(line); m != nil {
			out[m[2]] = m[1]
		}
	}
	return out
}

// lineNaming returns the first banner line containing name, or "".
// 🔴 THE MATCH IS ON A WHOLE VARIABLE NAME, NOT A SUBSTRING, AND THE DIFFERENCE IS
// A MEASURED HOLE. A plain strings.Contains is satisfied by any LONGER variable that
// starts with the name: the moment MUSTER_AGENT_GATEWAY_MODEL joined config.go, a
// banner line naming only the MODEL variable made every both-directions assertion
// about MUSTER_AGENT_GATEWAY pass. Found by mutation — blanking the gateway variable
// out of the CHAT: WIRED line SURVIVED, because the sentinel beside it still spelled
// the shorter name as a prefix — so this is the same prefix-collision class
// tests/leakscan.py records for its own identifier walk, in a different instrument.
//
// ⚠ A VARIABLE NAME IS THE MAXIMAL RUN OF [A-Z0-9_], so the boundary test is "the
// character after the match is not one of those". It is not a word boundary in the
// regexp sense: `_` is a word character there, which is exactly the case that has to
// fail to match.
func lineNaming(banner, name string) string {
	for _, line := range strings.Split(banner, "\n") {
		for i := 0; ; {
			j := strings.Index(line[i:], name)
			if j < 0 {
				break
			}
			end := i + j + len(name)
			if end == len(line) || !isEnvNameByte(line[end]) {
				return line
			}
			i = end
		}
	}
	return ""
}

// isEnvNameByte reports whether b can appear inside an environment variable name,
// i.e. whether a match ending before it is really the end of that name.
func isEnvNameByte(b byte) bool {
	return b == '_' || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// TestTheProvisioningSeamLineDoesNotClaimAnUnnamedRefusal pins the corrected
// agent-provisioning line.
//
// 🔴 THE OLD LINE SAID THE ROUTES "REFUSE" AND NINE OF THEM ANSWERED 200. A
// claim in a boot banner is the strongest documentation this service emits —
// it is what an operator reads while debugging — so it is worth pinning to a
// symbol a reader can go and check rather than to an adjective. The line now
// cites both wrapper symbols and the discriminator field, all of which are real
// and all of which a grep resolves.
//
// 🔴 IT REQUIRES BOTH WRAPPERS NOW, WHICH IS STRICTLY STRONGER THAN THE ONE NAME IT
// USED TO CHECK. api's combined Provisioner was split into Provisioner (lifecycle)
// and Gateway (chat), each with its own wrapper. A banner naming only one would be
// true about seven routes and silent about two — and the two it omitted would be
// exactly the ones a reader had no way to know were separately gated. Naming one
// wrapper was also what this test caught when the split landed: the old expectation
// went red because the banner had stopped citing a symbol that still existed, which
// is the behaviour it was written for.
func TestTheProvisioningSeamLineDoesNotClaimAnUnnamedRefusal(t *testing.T) {
	out := renderBanner(t, config{}, api.Extensions{})
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "agent provisioning:") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("the banner no longer announces the provisioning seam at all.\n%s", out)
	}
	for _, want := range []string{
		"requireLifecycleProvisioner",
		"requireGatewayProvisioner",
		api.ProvisionerUnwiredField,
		"503",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the agent-provisioning banner line does not name %q.\n"+
				"    This line asserted for the whole of the previous revision that every "+
				"agent-control route \"refuses at request time\", while nine of them "+
				"answered 200 and did nothing. The claim is only checkable if it names "+
				"the mechanism, so do not soften it back to an unqualified \"refuses\" — "+
				"and there are TWO mechanisms now, so naming one is a claim about seven "+
				"routes and a silence about two.\n"+
				"  line: %s", want, line)
		}
	}
}

// TestTheHalfWiredProvisioningBannerReportsBothTiersSeparately is the guard for the
// state the capability split made reachable and nothing else describes.
//
// 🔴 THE ONE-LINE BANNER WAS TRUE ONLY WHILE BOTH HALVES WERE ALWAYS UNWIRED
// TOGETHER. With a lifecycle provisioner wired and no api.Gateway, a single
// sentence about "agent provisioning" has to pick a half, and either choice is a
// false claim about the other — the same defect
// TestTheProvisioningSeamLineDoesNotClaimAnUnnamedRefusal was written for, in the
// shape the split created.
//
// 🔴 IT PINS WHERE THE UNDELIVERED KICKOFF IS RECORDED, NOT JUST THAT CHAT IS OFF.
// This deployment's surprising behaviour is that a dispatch SUCCEEDS, a pod runs,
// logs stream — and the agent was never told what to do. An operator reading only
// "chat unwired" would not know to look at agents.kickoff_error, and the card looks
// healthy. That field name is the actionable half of the line.
//
// ⚠ IT WAS WRITTEN BECAUSE A MUTANT SURVIVED. The existing seam-line test matches
// any line containing "agent provisioning:", so renaming the wired branch's lines
// left it green — it only ever renders the both-nil arm.
func TestTheHalfWiredProvisioningBannerReportsBothTiersSeparately(t *testing.T) {
	cfg := config{
		AgentProvisioner: provisionerNoop,
		AgentImageRepo:   "registry.example.test/muster/agent-runtime",
		AgentAPIURL:      "http://muster.example.test:8105",
	}
	out := renderBanner(t, cfg, api.Extensions{Provisioner: bannerProvisioner(t, cfg)})

	lifecycle := lineNaming(out, "LIFECYCLE")
	if lifecycle == "" {
		t.Fatalf("the banner does not report the LIFECYCLE tier with a provisioner wired, so "+
			"an operator cannot tell this deployment can provision.\nbanner:\n%s", out)
	}
	for _, want := range []string{"WIRED", envAgentProvisioner, provisionerNoop,
		"requireLifecycleProvisioner"} {
		if !strings.Contains(lifecycle, want) {
			t.Errorf("the LIFECYCLE line does not name %q.\n  line: %s", want, lifecycle)
		}
	}
	if strings.Contains(lifecycle, "UNWIRED") {
		t.Errorf("the LIFECYCLE line says UNWIRED while a provisioner IS wired — wiredWord's "+
			"argument is inverted.\n  line: %s", lifecycle)
	}

	chat := lineNaming(out, "CHAT")
	if chat == "" {
		t.Fatalf("the banner says nothing about the CHAT tier while it is UNWIRED and "+
			"lifecycle is not. That is the half an operator is surprised by: the dispatch "+
			"works and the agent is never told what to do.\nbanner:\n%s", out)
	}
	for _, want := range []string{"UNWIRED", "requireGatewayProvisioner",
		api.ProvisionerUnwiredField, "agents.kickoff_error", "agents.pending_note"} {
		if !strings.Contains(chat, want) {
			t.Errorf("the CHAT line does not name %q.\n"+
				"    Naming the FIELD the non-delivery is recorded in is the actionable "+
				"half: without it the card reads healthy and nothing points at the "+
				"reason.\n  line: %s", want, chat)
		}
	}
}

// bannerVAPIDPriv / bannerVAPIDPub are a keypair minted when the test binary
// starts, so no key material is written into the tree.
var bannerVAPIDPriv, bannerVAPIDPub = func() (string, string) {
	priv, pub, err := push.GenerateVAPIDKeys()
	if err != nil {
		panic(err)
	}
	return priv, pub
}()

// bannerUploads is the REAL builder over cfg (with a pool), so the ON render
// is the arm a deployment with that configuration would print.
func bannerUploads(t *testing.T, cfg config) *uploads.Service {
	t.Helper()
	svc := buildUploads(cfg, &pgxpool.Pool{}, log.New(io.Discard, "", 0))
	if svc == nil {
		t.Fatalf("buildUploads = nil for the ON fixture: %s", parseUploads(cfg, true).off)
	}
	return svc
}

// bannerTranscriber is the REAL builder over cfg, so the ON render is the arm a
// deployment with that configuration would print.
func bannerTranscriber(t *testing.T, cfg config) api.Transcriber {
	t.Helper()
	tr, err := buildTranscriber(cfg)
	if err != nil || tr == nil {
		t.Fatalf("buildTranscriber(%+v) = %v, %v; want a client", cfg.STTURL, tr, err)
	}
	return tr
}
