package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The image's REAL template, not a copy: the seed and the hook wiring are only
// as right as the file the image ships.
const imageSettingsTemplate = "../../images/claude-code-agent/settings.json"

func readObj(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func seedInto(t *testing.T, cfg, ws string) {
	t.Helper()
	var stderr bytes.Buffer
	if err := seed(seedOpts{configDir: cfg, workspace: ws, settingsTemplate: imageSettingsTemplate}, &stderr); err != nil {
		t.Fatal(err)
	}
}

// imageHooks is the LEDGER of the hooks the image registers: event -> matcher and
// command. 🔴 It fails when the set GROWS (a new event ccd would receive unasked)
// or SHRINKS (dropping SessionStart leaves /healthz never ready; dropping
// SessionEnd leaves ccd pasting into a TUI that has exited; dropping StopFailure
// leaves a failed turn waiting out the whole turn timeout; dropping
// PermissionRequest silently stops routing prompts to the operator's phone;
// dropping Notification or PostToolUse leaves the "awaiting approval" badge
// never set or never cleared).
var imageHooks = map[string]struct{ matcher, command string }{
	"SessionStart":      {"", "ccd hook SessionStart"},
	"SessionEnd":        {"", "ccd hook SessionEnd"},
	"UserPromptSubmit":  {"", "ccd hook UserPromptSubmit"},
	"Stop":              {"", "ccd hook Stop"},
	"StopFailure":       {"", "ccd hook StopFailure"},
	"PermissionRequest": {"", "ccd approve"},
	"Notification":      {"permission_prompt", "ccd hook Notification"},
	"PostToolUse":       {"", "ccd hook PostToolUse"},
}

func TestTheImageRegistersExactlyTheHooksCcdNeeds(t *testing.T) {
	hooks, _ := readObj(t, imageSettingsTemplate)["hooks"].(map[string]any)
	var events, want []string
	for ev := range imageHooks {
		want = append(want, ev)
	}
	for ev, v := range hooks {
		events = append(events, ev)
		groups, _ := v.([]any)
		if len(groups) != 1 {
			t.Fatalf("%s: %d matcher groups, want 1", ev, len(groups))
		}
		group := groups[0].(map[string]any)
		hs, _ := group["hooks"].([]any)
		if len(hs) != 1 {
			t.Fatalf("%s: %d hooks, want 1", ev, len(hs))
		}
		h := hs[0].(map[string]any)
		exp, known := imageHooks[ev]
		if !known {
			continue // reported by the set comparison below
		}
		if h["type"] != "command" || h["command"] != exp.command {
			t.Fatalf("%s: hook %v, want command `%s`", ev, h, exp.command)
		}
		if m, _ := group["matcher"].(string); m != exp.matcher {
			t.Fatalf("%s: matcher %q, want %q", ev, m, exp.matcher)
		}
	}
	sort.Strings(events)
	sort.Strings(want)
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("registered events %v, want exactly %v", events, want)
	}
}

// 🔴 THE PERMISSION POSTURE IS PINNED, WHOLE. The operator's decision for these
// pods is "default permissions + approvals, never bypass". The pinned CLI makes
// AUTO mode its own default ("auto mode on (shift+tab to cycle)" was measured in
// a live pod whose settings named no mode at all), so silence here is not
// default mode. defaultMode makes the starting mode explicit, disableAutoMode
// removes auto from the cycle, disableBypassPermissionsMode removes bypass. The
// whole object is compared so a removed OR an added key (an `allow` list, say)
// fails.
func TestTheImagePinsDefaultPermissionModeNeverAutoNeverBypass(t *testing.T) {
	perms, ok := readObj(t, imageSettingsTemplate)["permissions"].(map[string]any)
	if !ok {
		t.Fatal("settings template has no `permissions` object: the CLI would start in its own default, which is auto mode")
	}
	got, _ := json.Marshal(perms)
	const want = `{"defaultMode":"default","disableAutoMode":"disable","disableBypassPermissionsMode":"disable"}`
	if string(got) != want {
		t.Fatalf("permissions = %s\nwant          %s", got, want)
	}
}

// The posture must survive a seed over a persisted settings.json that an
// operator's /config (or the session) left in auto or bypass mode: the template's
// top-level `permissions` replaces it wholesale.
//
// 🔴 TWO CASES, BOTH PINNED WHOLE. CCD_JOB_PERMISSION_MODE unset is the default
// posture — bypass DISABLED and no disclaimer consent, even over a file a
// bypass posture left behind; set to bypassPermissions it lifts ONLY
// disableBypassPermissionsMode (defaultMode and disableAutoMode stay) and records
// the consent the pinned CLI's launch dialog needs (jobmode.go). checkSeed agrees
// with each, and refuses the other's file.
func TestSeedReimposesThePermissionPostureOverAPersistedAutoMode(t *testing.T) {
	for _, c := range []struct {
		name, mode, prior, wantPerms string
		wantSkip                     any // settingsSkipBypassPrompt's value; nil = absent
	}{
		{"default posture", "",
			`{"permissions":{"defaultMode":"bypassPermissions","allow":["Bash(*)"]},"skipDangerousModePermissionPrompt":true,"model":"kept"}`,
			`{"defaultMode":"default","disableAutoMode":"disable","disableBypassPermissionsMode":"disable"}`, nil},
		{"job-lane bypass", "bypassPermissions",
			`{"permissions":{"defaultMode":"auto","allow":["Bash(*)"]},"model":"kept"}`,
			`{"defaultMode":"default","disableAutoMode":"disable"}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "ws")
			if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(c.prior), 0o600); err != nil {
				t.Fatal(err)
			}
			o := seedOpts{configDir: cfg, workspace: ws, settingsTemplate: imageSettingsTemplate, jobPermissionMode: c.mode}
			if err := seed(o, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			set := readObj(t, filepath.Join(cfg, "settings.json"))
			got, _ := json.Marshal(set["permissions"])
			if string(got) != c.wantPerms {
				t.Fatalf("after seed, permissions = %s\nwant                      %s", got, c.wantPerms)
			}
			if skip, ok := set["skipDangerousModePermissionPrompt"]; (c.wantSkip == nil && ok) || (c.wantSkip != nil && skip != c.wantSkip) {
				t.Fatalf("after seed, skipDangerousModePermissionPrompt = %v (present %v), want %v", skip, ok, c.wantSkip)
			}
			if set["model"] != "kept" {
				t.Fatalf("seed dropped an unrelated key: %v", set)
			}
			if err := checkSeed(o); err != nil {
				t.Fatalf("checkSeed disagrees with seed: %v", err)
			}
			other := o
			other.jobPermissionMode = map[string]string{"": "bypassPermissions", "bypassPermissions": ""}[c.mode]
			if err := checkSeed(other); err == nil {
				t.Fatalf("checkSeed with mode %q accepted the file seeded with mode %q", other.jobPermissionMode, c.mode)
			}
		})
	}
}

// 🔴 FAIL CLOSED: a CCD_JOB_PERMISSION_MODE that is neither unset nor EXACTLY
// bypassPermissions is refused before anything is written — never read as
// bypass, never silently dropped.
func TestSeedRefusesAnyOtherJobPermissionMode(t *testing.T) {
	for _, v := range []string{"bypass", "BypassPermissions", " bypassPermissions", "bypassPermissions\n", "true", "1", "dontAsk", "acceptEdits", "default"} {
		cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "ws")
		err := seed(seedOpts{configDir: cfg, workspace: ws, settingsTemplate: imageSettingsTemplate, jobPermissionMode: v}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "CCD_JOB_PERMISSION_MODE") {
			t.Errorf("%q: err = %v, want a refusal naming CCD_JOB_PERMISSION_MODE", v, err)
		}
		for _, f := range []string{".claude.json", "settings.json"} {
			if _, serr := os.Stat(filepath.Join(cfg, f)); serr == nil {
				t.Errorf("%q: seed wrote %s before refusing", v, f)
			}
		}
	}
}

// `ccd seed` takes the mode from the pod's environment (the entrypoint passes
// no flag for it): set, the bypass refusal is lifted; unset, it stays.
func TestRunSeedReadsTheJobPermissionModeFromTheEnvironment(t *testing.T) {
	for _, c := range []struct {
		env         string
		wantDisable bool
	}{{"", true}, {"bypassPermissions", false}} {
		cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "ws")
		t.Setenv(envJobPermissionMode, c.env)
		var out, errb bytes.Buffer
		if rc := runSeed([]string{"--config-dir", cfg, "--workspace", ws, "--settings-template", imageSettingsTemplate}, &out, &errb); rc != 0 {
			t.Fatalf("env %q: rc %d: %s", c.env, rc, errb.String())
		}
		perms, _ := readObj(t, filepath.Join(cfg, "settings.json"))["permissions"].(map[string]any)
		if _, has := perms["disableBypassPermissionsMode"]; has != c.wantDisable {
			t.Fatalf("env %q: disableBypassPermissionsMode present = %v, want %v", c.env, has, c.wantDisable)
		}
	}
}

// The PRIMARY supervisor never starts the CLI with a permission flag: its mode
// comes from settings alone, so nothing on argv can widen it (a job lane on an
// opted-in pool is the one exception, jobmode_test.go). ⚠ INVARIANT GUARD (the
// argv never carried one), and it checks the fresh-start argv only.
func TestTheSupervisorPassesNoPermissionFlag(t *testing.T) {
	sup := newSupervisor(nil, "claude", t.TempDir(), t.TempDir())
	argv, _ := sup.argv()
	for _, a := range argv {
		if strings.Contains(a, "permission") || strings.Contains(a, "dangerously") {
			t.Fatalf("supervisor argv %v carries a permission flag", argv)
		}
	}
}

func TestSeedOnAnEmptyDirSkipsOnboardingAndTrustsTheWorkspace(t *testing.T) {
	cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "workspace")
	seedInto(t, cfg, ws)
	st := readObj(t, filepath.Join(cfg, ".claude.json"))
	if st["hasCompletedOnboarding"] != true || st["theme"] != "dark" {
		t.Fatalf(".claude.json: %v", st)
	}
	proj := st["projects"].(map[string]any)[ws].(map[string]any)
	if proj["hasTrustDialogAccepted"] != true {
		t.Fatalf("trust: %v", proj)
	}
	if fi, err := os.Stat(filepath.Join(cfg, ".claude.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf(".claude.json mode: %v %v", fi.Mode(), err)
	}
	if _, err := os.Stat(ws); err != nil {
		t.Fatal("workspace not created")
	}
	if err := checkSeed(seedOpts{configDir: cfg, workspace: ws, settingsTemplate: imageSettingsTemplate}); err != nil {
		t.Fatal(err)
	}
}

// On a persistent volume the CLI has written its own state into .claude.json;
// re-seeding at the next start must keep it.
func TestReseedPreservesTheCLIsOwnState(t *testing.T) {
	cfg, ws := t.TempDir(), "/data/workspace-test"
	ws = filepath.Join(t.TempDir(), "ws")
	prior := map[string]any{
		"numStartups": 7.0, "theme": "light",
		"projects": map[string]any{
			ws:       map[string]any{"lastSessionId": "abc", "hasTrustDialogAccepted": false},
			"/other": map[string]any{"hasTrustDialogAccepted": true},
		},
	}
	b, _ := json.Marshal(prior)
	if err := os.WriteFile(filepath.Join(cfg, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"model":"opus","hooks":{"PreToolUse":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	seedInto(t, cfg, ws)
	st := readObj(t, filepath.Join(cfg, ".claude.json"))
	projects := st["projects"].(map[string]any)
	mine := projects[ws].(map[string]any)
	if st["numStartups"] != 7.0 || st["theme"] != "light" || mine["lastSessionId"] != "abc" ||
		mine["hasTrustDialogAccepted"] != true || projects["/other"] == nil {
		t.Fatalf("state after reseed: %v", st)
	}
	set := readObj(t, filepath.Join(cfg, "settings.json"))
	if set["model"] != "opus" {
		t.Fatal("a settings key the template does not name was dropped")
	}
	if _, ok := set["hooks"].(map[string]any)["PreToolUse"]; ok {
		t.Fatal("the template's `hooks` did not replace the file's (it is image-owned)")
	}
}

func TestSeedMovesACorruptStateFileAside(t *testing.T) {
	cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "ws")
	if err := os.WriteFile(filepath.Join(cfg, ".claude.json"), []byte(`{truncated`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if err := seed(seedOpts{configDir: cfg, workspace: ws, settingsTemplate: imageSettingsTemplate}, &stderr); err != nil {
		t.Fatal(err)
	}
	m, _ := filepath.Glob(filepath.Join(cfg, ".claude.json.corrupt-*"))
	if len(m) != 1 || !strings.Contains(stderr.String(), "moved to") {
		t.Fatalf("aside=%v stderr=%q", m, stderr.String())
	}
	if readObj(t, filepath.Join(cfg, ".claude.json"))["hasCompletedOnboarding"] != true {
		t.Fatal("not reseeded after moving the corrupt file aside")
	}
}

func TestCheckSeedCatchesEachMissingPiece(t *testing.T) {
	cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "ws")
	o := seedOpts{configDir: cfg, workspace: ws, settingsTemplate: imageSettingsTemplate}
	seedInto(t, cfg, ws)
	if err := checkSeed(o); err != nil {
		t.Fatalf("control: %v", err)
	}
	mutate := func(file string, f func(map[string]any)) {
		m := readObj(t, filepath.Join(cfg, file))
		f(m)
		b, _ := json.Marshal(m)
		if err := os.WriteFile(filepath.Join(cfg, file), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mutate(".claude.json", func(m map[string]any) { m["hasCompletedOnboarding"] = false })
	if err := checkSeed(o); err == nil || !strings.Contains(err.Error(), "theme picker") {
		t.Fatalf("onboarding: %v", err)
	}
	seedInto(t, cfg, ws)
	mutate(".claude.json", func(m map[string]any) { delete(m["projects"].(map[string]any), ws) })
	if err := checkSeed(o); err == nil || !strings.Contains(err.Error(), "trust dialog") {
		t.Fatalf("trust: %v", err)
	}
	seedInto(t, cfg, ws)
	mutate("settings.json", func(m map[string]any) { delete(m, "hooks") })
	if err := checkSeed(o); err == nil || !strings.Contains(err.Error(), `"hooks"`) {
		t.Fatalf("hooks: %v", err)
	}
}

func TestSeedRefusesARelativeWorkspace(t *testing.T) {
	err := seed(seedOpts{configDir: t.TempDir(), workspace: "relative/ws", settingsTemplate: imageSettingsTemplate}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "exact absolute cwd") {
		t.Fatalf("err = %v", err)
	}
}

// 🔴 F5: seed removes `env` and `apiKeyHelper` from the persisted settings.json —
// either can put a credential or endpoint in front of the subscription token —
// and keeps every other key the operator set. --check refuses a file carrying
// either.
func TestSeedRemovesEnvAndAPIKeyHelperAndKeepsOtherKeys(t *testing.T) {
	cfg, ws := t.TempDir(), filepath.Join(t.TempDir(), "workspace")
	prior := `{"env":{"ANTHROPIC_BASE_URL":"https://fake.invalid","ANTHROPIC_API_KEY":"sk-ant-fake"},` +
		`"apiKeyHelper":"/bin/echo sk-ant-fake","model":"operator-choice"}`
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if err := seed(seedOpts{configDir: cfg, workspace: ws, settingsTemplate: imageSettingsTemplate}, &stderr); err != nil {
		t.Fatal(err)
	}
	got := readObj(t, filepath.Join(cfg, "settings.json"))
	for _, k := range []string{"env", "apiKeyHelper"} {
		if _, ok := got[k]; ok {
			t.Fatalf("settings.json still carries %q: %v", k, got)
		}
		if !strings.Contains(stderr.String(), `removed "`+k+`"`) {
			t.Fatalf("seed did not say it removed %q: %s", k, stderr.String())
		}
	}
	if got["model"] != "operator-choice" || got["hooks"] == nil {
		t.Fatalf("seed dropped a key it should keep: %v", got)
	}
	if strings.Contains(stderr.String(), "sk-ant-fake") {
		t.Fatalf("seed printed a removed VALUE: %s", stderr.String())
	}
	o := seedOpts{configDir: cfg, workspace: ws, settingsTemplate: imageSettingsTemplate}
	if err := checkSeed(o); err != nil {
		t.Fatalf("check after seed: %v", err)
	}
	for _, k := range []string{"env", "apiKeyHelper"} {
		bad := readObj(t, filepath.Join(cfg, "settings.json"))
		bad[k] = "x"
		b, _ := json.Marshal(bad)
		if err := os.WriteFile(filepath.Join(cfg, "settings.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := checkSeed(o); err == nil || !strings.Contains(err.Error(), k) {
			t.Fatalf("check with %q present: %v", k, err)
		}
		delete(bad, k)
		b, _ = json.Marshal(bad)
		_ = os.WriteFile(filepath.Join(cfg, "settings.json"), b, 0o600)
	}
}
