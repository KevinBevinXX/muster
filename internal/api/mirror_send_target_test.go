package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// What mirror_terminal_send_test.go and stepup_passkey_test.go cannot say with
// literals alone, because it is about types this change introduced: which PANE
// each send route asks the plugin for, an older muster-mirror, and a CONFIGURED
// window.

// TestEachSendRouteAsksForItsOwnPane: 🔴 the two routes are one handler and
// differ in exactly this. The rendered view's route asks for the Claude pane
// ONLY — so a prompt written for Claude can never be typed into the shell that
// replaced it — and the Terminal view's asks for the active pane.
func TestEachSendRouteAsksForItsOwnPane(t *testing.T) {
	for path, want := range map[string]mirror.PasteTarget{
		ui.MirrorSendPath:         mirror.PasteClaude,
		ui.MirrorTerminalSendPath: mirror.PasteActivePane,
	} {
		fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}}
		s, ts := viewServer(t, fm)
		code, body, _ := postSend(t, ts.URL, path, sessionCookieFor(s), "", url.Values{"host": {"ws"}, "session": {"main"}, "text": {"hi"}})
		if code != http.StatusNoContent {
			t.Fatalf("%s: %d %q", path, code, body)
		}
		_, pastes := fm.v2.snapshot()
		if len(pastes) != 1 || pastes[0].target != want {
			t.Errorf("%s asked the plugin for target %v (pastes %+v), want %v", path, pastes[0].target, pastes, want)
		}
	}
	if ui.MirrorSendPath == ui.MirrorTerminalSendPath || mirror.PasteClaude == mirror.PasteActivePane {
		t.Fatal("the two routes, or the two targets, are no longer distinct")
	}
}

// TestAnOlderMirrorDrawsTheComposerDisabledWhereItCannotPaste: an -interactive
// host whose muster-mirror predates pasting into any pane. Its Claude session's
// Terminal page has a working composer (the Claude-only paste reaches that
// pane); its other sessions' draw it DISABLED, saying to upgrade — not missing,
// which would look like the feature does not exist. And the plugin's own
// refusal of such a send is a 409 carrying that reason.
func TestAnOlderMirrorDrawsTheComposerDisabledWhereItCannotPaste(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true, oldMirror: true, v2: &fakeMirrorV2{}}
	s, ts := viewServer(t, fm)
	_, agent := mirrorGet(t, s, ts, ui.MirrorTerminalPath("ws", "agent"))
	input, send := tagOf(agent, "mv-input"), tagOf(agent, "mv-send")
	if !disabledAttr.MatchString(input) || !disabledAttr.MatchString(send) {
		t.Fatalf("an older mirror's non-Claude session: the composer is not disabled: %s / %s", input, send)
	}
	if !strings.Contains(agent, "upgrade it to send into this session") || !strings.Contains(input, `placeholder="Update muster-mirror to send"`) {
		t.Error("the disabled composer does not say why")
	}
	_, main := mirrorGet(t, s, ts, ui.MirrorTerminalPath("ws", "main"))
	if in := tagOf(main, "mv-input"); in == "" || disabledAttr.MatchString(in) {
		t.Errorf("an older mirror's Claude session: the composer should work: %s", in)
	}

	fm.v2.pasteErr = mirror.ErrPanePasteUnsupported
	code, body, _ := postSend(t, ts.URL, ui.MirrorTerminalSendPath, sessionCookieFor(s), "", url.Values{"host": {"ws"}, "session": {"agent"}, "text": {"ls"}})
	if code != http.StatusConflict || !strings.Contains(body, "upgrade") {
		t.Fatalf("the plugin's own refusal: %d %q, want 409 naming the upgrade", code, body)
	}
}

// configureWindow sets the step-up window for one test and restores the
// default. No test in this package runs in parallel: the window is process-wide.
func configureWindow(t *testing.T, d time.Duration) {
	t.Helper()
	if err := stepup.SetWindow(d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stepup.SetWindow(stepup.DefaultWindow) })
}

// TestAConfiguredWindowIsTheOneTheGateAndThePageUse: MUSTER_STEPUP_WINDOW's
// value (30 minutes here), at the gate and in every sentence — on the fake
// GoTrue's injected clock. 29 minutes after sign-in is fresh, 31 is stale; the
// refusal, the dialog and the confirm's answer all say 30 minutes; nothing
// says 12 hours.
func TestAConfiguredWindowIsTheOneTheGateAndThePageUse(t *testing.T) {
	configureWindow(t, 30*time.Minute)
	r := newStepUpRig(t)
	r.mirror.v2 = &fakeMirrorV2{}
	at, rt, _ := r.fake.Mint(r.owner)
	r.fake.Advance(29 * time.Minute)
	if got := r.mustAttach(t, at, rt, "rw"); got != "rw" {
		t.Fatalf("29 minutes after sign-in under a 30 minute window: attached %q", got)
	}
	if w := r.send(at, rt, "in time"); w.Code != http.StatusNoContent {
		t.Fatalf("29 minutes: send %d %s", w.Code, w.Body)
	}
	r.fake.Advance(2 * time.Minute)
	r.mustRefuse(t, at, rt, "rw", http.StatusForbidden, "31 minutes after sign-in under a 30 minute window")
	w := r.send(at, rt, "too late")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "from the last 30 minutes") || strings.Contains(w.Body.String(), "12 hours") {
		t.Fatalf("31 minutes: send %d %q, want 403 naming 30 minutes", w.Code, w.Body)
	}
	page := do(r.edge, http.MethodGet, ui.MirrorTerminalPath("ws", "main"), nil, withCookies(at, rt), asNavigation).Body.String()
	if !strings.Contains(page, "from the last 30 minutes.") || strings.Contains(page, "12 hours") {
		t.Fatal("the dialog does not name the configured window")
	}
	c := r.confirm(at, rt, url.Values{"password": {stepUpOwnerPW}})
	var res stepUpResult
	if err := json.Unmarshal(c.Body.Bytes(), &res); err != nil {
		t.Fatalf("confirm answered %s", c.Body)
	}
	if c.Code != http.StatusOK || res.FreshFor <= (30*time.Minute).Milliseconds()-2000 || res.FreshFor > (30*time.Minute).Milliseconds() {
		t.Fatalf("confirm under a 30 minute window: %d fresh_for=%d, want within 2s of %d", c.Code, res.FreshFor, (30 * time.Minute).Milliseconds())
	}
}

// TestAddingAPasskeyStaysOnItsFiveMinutesWhateverTheWindow: 🔴 lengthening the
// typing window must not lengthen the one that guards a NEW CREDENTIAL. With
// the typing window at its maximum (24 hours), six minutes after sign-in the
// session may still type into a mirrored session — and may NOT add a passkey.
func TestAddingAPasskeyStaysOnItsFiveMinutesWhateverTheWindow(t *testing.T) {
	configureWindow(t, stepup.MaxWindow)
	w := newPasskeyWorld(t)
	_, sub, at, rt := w.joinMember("pk-window@invite.test")
	cookies := withCookies(at, rt)
	w.fake.Advance(4 * time.Minute)
	if rec := postJSON(w.edge, accountPasskeysPath+"/options", map[string]any{}, cookies); rec.Code != http.StatusOK {
		t.Fatalf("control: 4 minutes after sign-in: %d %s", rec.Code, rec.Body)
	}
	w.fake.Advance(2 * time.Minute)
	if rec := postJSON(w.edge, accountPasskeysPath+"/options", map[string]any{}, cookies); rec.Code != http.StatusUnauthorized || jsonBody(t, rec)["error"] != PasskeyReauthRequired {
		t.Fatalf("6 minutes after sign-in, with a 24h typing window, options: %d %s, want 401 reauth_required", rec.Code, rec.Body)
	}
	rec := postJSON(w.edge, accountPasskeysPath+"/verify", map[string]any{
		"challenge_id": w.fake.NewChallenge(sub), "credential": gotruetest.Attestation("late-cred", gotruetest.FlagUPUV)}, cookies)
	if rec.Code != http.StatusUnauthorized || len(w.fake.Passkeys(sub)) != 0 {
		t.Fatalf("6 minutes after sign-in, verify: %d %s; passkeys now %d", rec.Code, rec.Body, len(w.fake.Passkeys(sub)))
	}
}
