package mirrorhost

import (
	"regexp"
	"strings"
	"unicode"
)

// REDACTION BEFORE A PENDING ACTION LEAVES THE HOST (summarizeTool, PendingOf).
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

// redactedMark replaces each secret.
const redactedMark = "«redacted»"

var (
	reAuthHeader = regexp.MustCompile(`(?i)\b((?:proxy-)?authorization\s*[:=]\s*)(?:(?:bearer|basic|token|digest)\s+)?("[^"]*"|'[^']*'|[^\s'"]+)`)
	reBearer     = regexp.MustCompile(`(?i)\b(bearer|basic)\s+([A-Za-z0-9._~+/-]{6,}=*)`)
	reFlag       = regexp.MustCompile(`(?i)(^|\s)(-p|-u|--password|--passwd|--pass|--token|--access-token|--auth-token|--api-key|--apikey|--secret|--client-secret)(=|\s+)("[^"]*"|'[^']*'|[^\s'"]+)`)
	reAssign     = regexp.MustCompile(`(?i)\b([a-z0-9_]*(?:key|token|secret|password|passwd|pwd))=("[^"]*"|'[^']*'|[^\s'"&]+)`)
	reUserinfo   = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s@'"]+@`)
	reQuery      = regexp.MustCompile(`(?i)([?&;][a-z0-9_.-]*(?:token|key|secret|sig|signature|password|passwd|pass|auth)[a-z0-9_.-]*=)[^&\s#'"]+`)
	rePrefix     = regexp.MustCompile(`\b(?:gh[pousr]_|github_pat_|glpat-|xox[abposr]-|sk-|mst_|mmt_|mpl_)[A-Za-z0-9_-]{6,}|\bAKIA[A-Z0-9]{12,}`)
)

// redactSecrets is s with every value that looks like a secret replaced by
// «redacted» (see the comment above for the shapes).
func redactSecrets(s string) string {
	if s == "" {
		return s
	}
	s = reAuthHeader.ReplaceAllString(s, "${1}"+redactedMark)
	s = reBearer.ReplaceAllStringFunc(s, func(m string) string {
		sub := reBearer.FindStringSubmatch(m)
		if !strings.ContainsFunc(sub[2], unicode.IsDigit) {
			return m // "basic setup", not a credential
		}
		return sub[1] + " " + redactedMark
	})
	s = reFlag.ReplaceAllStringFunc(s, func(m string) string {
		sub := reFlag.FindStringSubmatch(m)
		if strings.EqualFold(sub[2], "-u") && !strings.Contains(sub[4], ":") {
			return m // -u without user:password (git add -u, ps -u root)
		}
		if strings.HasPrefix(sub[4], redactedMark) {
			return m
		}
		return sub[1] + sub[2] + sub[3] + redactedMark
	})
	s = reAssign.ReplaceAllString(s, "${1}="+redactedMark)
	s = reUserinfo.ReplaceAllString(s, "${1}"+redactedMark+"@")
	s = reQuery.ReplaceAllString(s, "${1}"+redactedMark)
	s = rePrefix.ReplaceAllString(s, redactedMark)
	return s
}
