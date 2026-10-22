package main

import "fmt"

// JOB-LANE PERMISSION BYPASS, OPT-IN PER WORKER POOL.
//
// By default every CLI ccd starts runs in default permission mode with bypass
// DISABLED by the seeded settings (images/claude-code-agent/settings.json), and a
// tool call that needs permission is routed to the operator (approve.go). A
// worker pool with no approval relay — machine review lanes nobody watches —
// cannot work that way: its job lanes would wait at a prompt until the turn
// times out. muster opts such a pool in (MUSTER_AGENT_CC_BYPASS_POOLS) by
// writing envJobPermissionMode=bypassPermissions into its agents' pods
// (internal/agentspec claudeCodeEnv), and ccd then:
//
//   - starts JOB lanes only (a lane with a workdir: a machine job's worktree,
//     lanes.newLane) with `--permission-mode bypassPermissions`. The primary
//     CLI and a lane in ccd's own workspace (an explicit chat session) keep the
//     default posture: their argv carries no permission flag;
//   - has `ccd seed` drop permissions.disableBypassPermissionsMode from the
//     settings it imposes (the CLI refuses the flag while it is set) and record
//     the bypass disclaimer's consent (settingsSkipBypassPrompt), without which
//     the pinned CLI opens an accept/decline dialog at launch that a pasted job
//     prompt would answer (seed.go jobPermissionSettings).
//
// 🔴 FAIL CLOSED. Any value other than unset (or empty) and EXACTLY
// jobPermissionBypass is an error: ccd refuses to start and `ccd seed` refuses
// to seed, so the pod crash-loops with the variable named. A typo never becomes
// bypass, and an attempt at bypass is never silently dropped either.
//
// ⚠ WHAT BYPASS MEANS FOR THE POD: a job lane can run any command on the code
// it reviews, as ccd's user, and so can read anything that user can — the
// agent's own Claude setup-token included. Every process in the pod runs as
// that user, so on a listed pool's agent the WHOLE POD should be treated as
// running without approvals, not just the lane.
const (
	envJobPermissionMode = "CCD_JOB_PERMISSION_MODE"
	jobPermissionBypass  = "bypassPermissions"
)

// jobBypass parses envJobPermissionMode's value: "" is the default posture,
// exactly jobPermissionBypass is bypass, and anything else is refused.
func jobBypass(v string) (bool, error) {
	switch v {
	case "":
		return false, nil
	case jobPermissionBypass:
		return true, nil
	}
	return false, fmt.Errorf("%s=%q is not a permission mode ccd applies: the only value is %q (job lanes "+
		"without approvals), and unset keeps every lane at default permissions. Refusing rather than guessing",
		envJobPermissionMode, v, jobPermissionBypass)
}
