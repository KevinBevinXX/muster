package hermeshost

import "strings"

// THE TERMINAL GATE'S DESCRIPTIONS (audit #212, round 2). The pinned Hermes
// asks for approval through more than one gate, and every one reaches the
// viewer with the same fields: a terminal command
// (P:tools/approval.py's check_dangerous_command / check_all_command_guards,
// whose description is detect_dangerous_command's), but also a file write
// guard (P:tools/file_tools_write_guards.py:389, "command" = the LABEL
// `<write to …/.ssh/config>`), a plugin's approval rule (`<tool> (plugin
// approval rule)`), computer use (`computer_use: …`) and execute_code. Only
// the first approves a command that IS what will run.
//
// So muster offers "once" only for an approval whose description is one the
// TERMINAL gate emits — this list, read from the pinned
// P:tools/approval_detection.py: every DANGEROUS_PATTERNS description,
// _PARSER_LIMIT_DESCRIPTION, _MALFORMED_EXEC_DESCRIPTION,
// _GATEWAY_LIFECYCLE_SPLICE_DESCRIPTION and the static execution-flag
// findings — AND whose command is a pending `terminal` call's command in the
// conversation (State.PendingTerminalCommands): an allowlist. (A plugin
// rule's description is plugin text and can be on it, so NonTerminalLabels
// below refuses the label shapes too — on top of the allowlist, never in
// place of it.) ("arbitrary program execution via …", built at
// run time, is not listed: such an approval is deny only, the safe way
// round.) TestTheTerminalGateLedgerMatchesThePin (livehermes) re-extracts the
// list from the pin and pins both source files by sha256, so a pin bump that
// adds a pattern or a gate is RED.
var TerminalDescriptions = map[string]bool{
	"PowerShell destructive delete (Remove-Item)":                                true,
	"PowerShell encoded command execution":                                       true,
	"SQL DELETE without WHERE":                                                   true,
	"SQL DROP":                                                                   true,
	"SQL TRUNCATE":                                                               true,
	"Windows PowerShell destructive delete":                                      true,
	"Windows cmd destructive delete":                                             true,
	"Windows destructive delete (recursive/quiet switch)":                        true,
	"access to Hermes secrets (Windows path)":                                    true,
	"access to SSH keys (Windows path)":                                          true,
	"chmod +x followed by immediate execution":                                   true,
	"cloud metadata endpoint access (instance credentials)":                      true,
	"command parser limit exceeded":                                              true,
	"command parser limit or malformed executable payload":                       true,
	"copy/move file into sensitive credential/SSH/shell-rc path":                 true,
	"copy/move file into system config path":                                     true,
	"delete backups (wbadmin)":                                                   true,
	"delete in root path":                                                        true,
	"delete volume shadow copies (vssadmin)":                                     true,
	"disk copy":                                                                  true,
	"disk partitioning (diskpart)":                                               true,
	"docker compose restart/stop/kill/down (container lifecycle)":                true,
	"docker context use (switches default daemon for future commands)":           true,
	"docker restart/stop/kill (container lifecycle)":                             true,
	"docker with daemon redirect (--context: alternate daemon)":                  true,
	"docker with remote daemon redirect (-H/--host)":                             true,
	"docker/podman daemon redirect via environment (DOCKER_HOST/CONTAINER_HOST)": true,
	"dynamic shell word may expand to arbitrary program execution flag":          true,
	"execute remote content via Invoke-Expression":                               true,
	"execute remote content via command substitution":                            true,
	"execute remote script via process substitution":                             true,
	"find -delete":           true,
	"find -exec/-execdir rm": true,
	"find dynamic shell word may expand to destructive flag": true,
	"force kill processes":                       true,
	"force kill processes (Stop-Process -Force)": true,
	"force kill processes (killall -KILL)":       true,
	"force kill processes (killall -s KILL)":     true,
	"force kill processes (taskkill /F)":         true,
	"force stop service (Stop-Service -Force)":   true,
	"fork bomb":                                                                     true,
	"format drive (format.com)":                                                     true,
	"format filesystem":                                                             true,
	"format filesystem (Format-Volume)":                                             true,
	"git branch force delete":                                                       true,
	"git branch force delete (long flags)":                                          true,
	"git branch force delete (long flags, force-first)":                             true,
	"git clean with force (deletes untracked files)":                                true,
	"git force push (rewrites remote history)":                                      true,
	"git force push short flag (rewrites remote history)":                           true,
	"git reset --hard (destroys uncommitted changes)":                               true,
	"grant Everyone access (icacls)":                                                true,
	"hermes update (restarts gateway, kills running agents)":                        true,
	"in-place edit of Hermes config/env":                                            true,
	"in-place edit of Hermes config/env (long flag)":                                true,
	"in-place edit of Hermes config/env (perl/ruby)":                                true,
	"in-place edit of sensitive credential/SSH/shell-rc path":                       true,
	"in-place edit of sensitive credential/SSH/shell-rc path (long flag)":           true,
	"in-place edit of sensitive credential/SSH/shell-rc path (perl/ruby)":           true,
	"in-place edit of system config":                                                true,
	"in-place edit of system config (long flag)":                                    true,
	"invisible or bidirectional Unicode control character (possible obfuscation)":   true,
	"kill all processes":                                                            true,
	"kill hermes/gateway process (self-termination)":                                true,
	"kill process via backtick pgrep/pidof expansion (self-termination)":            true,
	"kill process via pgrep/pidof expansion (self-termination)":                     true,
	"kill processes by regex (killall -r)":                                          true,
	"modify boot configuration (bcdedit /set)":                                      true,
	"overwrite project env/config file":                                             true,
	"overwrite project env/config via redirection":                                  true,
	"overwrite project env/config via tee":                                          true,
	"overwrite system config":                                                       true,
	"overwrite system file via redirection":                                         true,
	"overwrite system file via tee":                                                 true,
	"package manager uninstall":                                                     true,
	"pipe a credential file into a curl/wget upload (possible exfiltration)":        true,
	"pipe decoded content to shell (possible command obfuscation)":                  true,
	"pipe openssl-decoded content to shell (possible command obfuscation)":          true,
	"pipe remote content to PowerShell (iwr | iex)":                                 true,
	"pipe remote content to shell":                                                  true,
	"pipe tr-transformed output to shell (possible command obfuscation)":            true,
	"pipe xxd-decoded content to shell (possible command obfuscation)":              true,
	"podman remote mode (-r/--remote: remote daemon)":                               true,
	"podman with remote daemon redirect (--url/--connection/--identity)":            true,
	"recursive chown to root":                                                       true,
	"recursive chown to root (long flag)":                                           true,
	"recursive delete":                                                              true,
	"recursive delete (flags after operands)":                                       true,
	"recursive delete (long flag)":                                                  true,
	"recursive world/other-writable (long flag)":                                    true,
	"registry delete (reg delete)":                                                  true,
	"registry value delete (Remove-ItemProperty -Force)":                            true,
	"reset ACLs recursively (icacls /reset)":                                        true,
	"script execution via -e/-c flag":                                               true,
	"script execution via heredoc":                                                  true,
	"shell command via -c/-lc flag":                                                 true,
	"shell execution via heredoc":                                                   true,
	"start gateway outside systemd (use 'systemctl --user restart hermes-gateway')": true,
	"stop/delete service (sc)":                                                      true,
	"stop/restart hermes gateway (kills running agents)":                            true,
	"stop/restart hermes gateway via shell-spliced verb (kills running agents)":     true,
	"stop/restart hermes launchd service (kills running agents)":                    true,
	"stop/restart system service":                                                   true,
	"sudo with combined-flag privilege escalation":                                  true,
	"sudo with privilege flag (stdin/askpass/shell/list)":                           true,
	"upload a secret or credential file via curl/wget (possible exfiltration)":      true,
	"wipe disk (Clear-Disk)":                                                        true,
	"wipe free space (cipher /w)":                                                   true,
	"world/other-writable permissions":                                              true,
	"write to block device":                                                         true,
	"xargs with rm":                                                                 true,
}

// THE NON-TERMINAL GATES' LABELS (audit #212, round 3). The description
// allowlist above is not enough on its own: a plugin's approval rule
// (P:tools/approval.py request_tool_approval, reached from
// P:hermes_cli/plugins.py's pre_tool_call) takes its description from the
// PLUGIN's reason — free text, which can equal a terminal description — and
// shows the label `<tool> (plugin approval rule)`. An injected model can then
// issue, in one batch, the plugin-gated call AND a `terminal` call whose
// command is that very label: both checks pass, and "once" would approve the
// plugin-gated call, which the card never showed. So a command shaped like
// any pinned non-terminal gate's label is never offered "once", whatever its
// description and whatever is pending.
//
// The shapes, read from the pin (TestTheApprovalGatesMatchThePin
// (livehermes) re-extracts every approval site and evaluates each label's
// f-string through NonTerminalLabel, so a pin bump that adds a gate or
// changes a label is RED):
//
//   - `<{tool_name}> (plugin approval rule)` — P:tools/approval.py
//     request_tool_approval;
//   - `<write to {…}>` — P:tools/file_tools_write_guards.py, the SSH-config
//     write guard and the protected agent-instruction write guard;
//   - `computer_use: {…}` — P:tools/computer_use/tool.py _request_approval;
//   - `execute_code <<'PY'\n…` — P:tools/approval.py
//     check_execute_code_guard (several lines, so never "once" anyway).
//
// The other non-terminal gates — an MCP elicitation, an untrusted MCP tool,
// a payment-card fill (P:tools/approval_prompt.py
// request_elicitation_consent) — show free text with a fixed-format
// description no terminal gate emits, so the description allowlist refuses
// them; and an `edit` approval carries a diff (ApprovalPrompt.Unshown).
var NonTerminalLabels = []LabelShape{
	{Prefix: "<", Suffix: "> (plugin approval rule)"},
	{Prefix: "<write to ", Suffix: ">"},
	{Prefix: "computer_use: "},
	{Prefix: "execute_code <<"},
}

// LabelShape is one non-terminal gate's label: a prefix and a suffix.
type LabelShape struct{ Prefix, Suffix string }

// NonTerminalLabel reports whether s has the shape of a label a pinned
// non-terminal gate shows in place of a command (NonTerminalLabels).
func NonTerminalLabel(s string) bool {
	for _, l := range NonTerminalLabels {
		if len(s) >= len(l.Prefix)+len(l.Suffix) && strings.HasPrefix(s, l.Prefix) && strings.HasSuffix(s, l.Suffix) {
			return true
		}
	}
	return false
}
