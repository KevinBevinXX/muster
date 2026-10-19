// Package redact is THE ONE secret redactor: what looks like a credential in a
// line of text an agent or a page will be shown is replaced by [Mark].
//
// 🔴 ONE COPY, EVERY CALLER. It was internal/mirrorhost's (the pending-action
// summary a waiting session sends, below) and moved here unchanged when the
// chief's sessions_overview began quoting session transcripts
// (internal/api's sessions_overview.go); the host also runs every field of a
// rendered view's transcript stream through it (internal/mirrorhost's
// redact_stream.go). A second redactor would drift from this one — the shapes
// it knows are a list that grows — so all of them call this.
package redact

import (
	"regexp"
	"strings"
	"unicode"
)

// REDACTION BEFORE A PENDING ACTION OR A RENDERED VIEW'S TRANSCRIPT LEAVES THE
// HOST (internal/mirrorhost's summarizeTool, PendingOf and redactLines) AND
// BEFORE sessions_overview QUOTES A TRANSCRIPT.
//
// What a waiting session waits on is shown on muster's queue page and handed
// to the chief's model by attention_list, so a command line pasted there must
// not carry the credential it was typed with. Every value that LOOKS like a
// secret is replaced by «redacted» here, on the host, before muster sees it:
//
//   - an Authorization / Proxy-Authorization header's value, and a Bearer or
//     Basic credential (one with a digit in it, so prose is left alone);
//   - the value of a -p, --password, --token (and kin) flag, and of a -u whose
//     value is user:password;
//   - an assignment to a name ending in KEY, TOKEN, SECRET, PASSWORD, PASSWD
//     or PWD (FOO_KEY=…, PGPASSWORD=…);
//   - a URL's userinfo, and its query parameters named token, key, secret,
//     sig/signature, password, pass or auth;
//   - a token with a well-known prefix: ghp_ (and gho_ ghu_ ghs_ ghr_),
//     github_pat_, glpat-, sk-, xoxb-/xoxp- (and kin), AKIA…, mst_, mmt_, mpl_.
//
// ⚠ A HEURISTIC, NOT A GUARANTEE: a secret in a shape this does not know
// passes. It is a second line behind the first, which is that only one line
// of a call's input is ever summarised, and that no push carries it.

// Mark replaces each secret.
const Mark = "«redacted»"

var (
	reAuthHeader = regexp.MustCompile(`(?i)\b((?:proxy-)?authorization\s*[:=]\s*)(?:(?:bearer|basic|token|digest)\s+)?("[^"]*"|'[^']*'|[^\s'"]+)`)
	reBearer     = regexp.MustCompile(`(?i)\b(bearer|basic)\s+([A-Za-z0-9._~+/-]{6,}=*)`)
	reFlag       = regexp.MustCompile(`(?i)(^|\s)(-p|-u|--password|--passwd|--pass|--token|--access-token|--auth-token|--api-key|--apikey|--secret|--client-secret)(=|\s+)("[^"]*"|'[^']*'|[^\s'"]+)`)
	reAssign     = regexp.MustCompile(`(?i)\b([a-z0-9_]*(?:key|token|secret|password|passwd|pwd))=("[^"]*"|'[^']*'|[^\s'"&]+)`)
	reUserinfo   = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s@'"]+@`)
	reQuery      = regexp.MustCompile(`(?i)([?&;][a-z0-9_.-]*(?:token|key|secret|sig|signature|password|passwd|pass|auth)[a-z0-9_.-]*=)[^&\s#'"]+`)
	rePrefix     = regexp.MustCompile(`\b(?:gh[pousr]_|github_pat_|glpat-|xox[abposr]-|sk-|mst_|mmt_|mpl_)[A-Za-z0-9_-]{6,}|\bAKIA[A-Z0-9]{12,}`)
)

// Secrets is s with every value that looks like a secret replaced by
// «redacted» (see the comment above for the shapes).
func Secrets(s string) string { return secrets(s, true) }

// secrets is Secrets; with filter false every pattern runs, which is what the
// test compares the filtered path against.
//
// 🔴 THE FILTER ONLY SKIPS A PATTERN THAT CANNOT MATCH. Go's regexp costs
// about 1 ms per pattern per 30 KiB of prose (measured at the rendered view's
// change: ~8 ms for all seven), and a rendered view's backfill is hundreds of
// such texts. Each pattern needs a literal of its own — "authorization",
// "bearer"/"basic", a flag's dash, a credential name followed by "=", "://"
// with an "@", a token prefix — so a text without that literal is not run
// through it. The literals are checked against the CURRENT text before each
// pattern (a replacement can remove one, never make one: «redacted» spells
// none), lowercased where the pattern ignores case.
func secrets(s string, filter bool) string {
	if s == "" {
		return s
	}
	lower := fold(s)
	may := func(lits ...string) bool {
		if !filter {
			return true
		}
		for _, l := range lits {
			if strings.Contains(lower, l) {
				return true
			}
		}
		return false
	}
	set := func(n string) {
		if n != s {
			s, lower = n, fold(n)
		}
	}
	if may("authorization") {
		set(reAuthHeader.ReplaceAllString(s, "${1}"+Mark))
	}
	if may("bearer", "basic") {
		set(reBearer.ReplaceAllStringFunc(s, func(m string) string {
			sub := reBearer.FindStringSubmatch(m)
			if !strings.ContainsFunc(sub[2], unicode.IsDigit) {
				return m // "basic setup", not a credential
			}
			return sub[1] + " " + Mark
		}))
	}
	if may("-p", "-u", "--") {
		set(reFlag.ReplaceAllStringFunc(s, func(m string) string {
			sub := reFlag.FindStringSubmatch(m)
			if strings.EqualFold(sub[2], "-u") && !strings.Contains(sub[4], ":") {
				return m // -u without user:password (git add -u, ps -u root)
			}
			if strings.HasPrefix(sub[4], Mark) {
				return m
			}
			return sub[1] + sub[2] + sub[3] + Mark
		}))
	}
	if may("key=", "token=", "secret=", "password=", "passwd=", "pwd=") {
		set(reAssign.ReplaceAllString(s, "${1}="+Mark))
	}
	if may("://") && strings.Contains(s, "@") {
		set(reUserinfo.ReplaceAllString(s, "${1}"+Mark+"@"))
	}
	if may("token", "key", "secret", "sig", "pass", "auth") && strings.Contains(s, "=") {
		set(reQuery.ReplaceAllString(s, "${1}"+Mark))
	}
	// Case-sensitive, as the pattern is.
	if !filter || containsAny(s, "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "glpat-",
		"xoxa-", "xoxb-", "xoxp-", "xoxo-", "xoxs-", "xoxr-", "sk-", "mst_", "mmt_", "mpl_", "AKIA") {
		set(rePrefix.ReplaceAllString(s, Mark))
	}
	return s
}

// fold is s lowercased the way the patterns' (?i) matches it: an ASCII
// letter's other case and, for k and s, the two non-ASCII letters Unicode
// folds with them (U+212A KELVIN SIGN, which ToLower already makes "k", and
// U+017F LONG S, which it leaves).
func fold(s string) string {
	return strings.Map(func(r rune) rune {
		if r = unicode.ToLower(r); r == 'ſ' {
			return 's'
		}
		return r
	}, s)
}

// containsAny reports whether s contains any of subs.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
