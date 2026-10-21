package hermeshost

import "strings"

// THE GATEWAY'S OWN MASKS (audit #212). Before an approval reaches any
// client, the pinned Hermes rewrites its command twice: redact_sensitive_text
// in the approval gate (P:tools/approval.py:855), then
// _redact_approval_command in the gateway (P:gateway/run.py:577, the same
// redactor with force=True). A mask can swallow shell syntax — measured:
// `ls -H X-Api-Key:a;rm -rf ~/work` arrives as `ls -H X-Api-Key:*** -rf
// ~/work` — so a command carrying ANY of its masks is not the command that
// runs, and muster never offers to approve it from a card.
//
// ⚠ A MASK IS NOT THE ONLY REWRITE: the redactor can also DROP characters
// without leaving one (a value of only control or zero-width characters
// becomes empty; edges are trimmed). That is caught by approvable's other
// rules, not here: the command must be printable ASCII and equal the RAW
// command of a pending terminal call in the conversation.
//
// The forms, read from the pinned agent/redact.py (the literals its mask
// functions emit); TestTheGatewayMasksCoverAPinnedSample (livehermes) pins
// that file by sha256 and runs a SAMPLE through both passes:
//
//   - "***"        the placeholder (mask_secret, _mask_token, the assignment,
//     header, URL and repr passes); "****" (a phone's middle)
//     contains it;
//   - "..."        mask_secret's head...tail form;
//   - "«redacted"  the non-reusable sentinels («redacted:…»,
//     «redacted-secret», «redacted-vault-secret»);
//   - "[REDACTED"  [REDACTED PRIVATE KEY].
//
// ("[redacted: nesting limit]" is the structure walker's, never a string
// redaction's: not listed.) ⚠ "..." also appears in commands nobody masked
// (`git diff a...b`): such a command is offered deny only — a false refusal,
// the safe way round.
var GatewayMasks = []string{"***", "...", "«redacted", "[REDACTED"}

// GatewayMasked reports whether s carries any of the gateway's masks.
func GatewayMasked(s string) bool {
	for _, m := range GatewayMasks {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
