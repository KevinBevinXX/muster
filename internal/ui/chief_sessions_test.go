package ui

import (
	"strings"
	"testing"
	"time"
)

func chiefPanelWithSessions(runtimes []SessionRuntimeView) ChiefPanelView {
	now := time.Now()
	return ChiefPanelView{
		Configured:      true,
		SessionRuntimes: runtimes,
		ClaudeModels:    []string{"opus", "sonnet"},
		Chat: AgentDetailView{ID: 105, Name: "chief", Surface: ChatSurfaceChiefPanel, ActiveSessionID: 92, Sessions: []SessionView{
			{ID: 92, Title: "the original", Active: true, LastActive: now, IsDefault: true, DefaultPath: "/agents/105/sessions/92/default"},
			{ID: 95, Title: "opus thread", LastActive: now, Runtime: "claude-code", RuntimeLabel: "Claude Code", Model: "opus",
				DefaultPath: "/agents/105/sessions/95/default"},
			{ID: 96, Title: "router thread", LastActive: now, Runtime: "gateway", RuntimeLabel: "OpenRouter",
				Model: "openrouter/vendor-x/model-y", DefaultPath: "/agents/105/sessions/96/default"},
		}},
	}
}

// TestThePanelPicksARuntimeAndModelAndMarksTheDefault: the thread list draws one
// "new session" form per offered runtime (each posting its runtime and a model
// to the session-create route, answered as the panel), each explicit thread's
// runtime and model, the Default badge on exactly the default thread, and
// "Make default" on every other one — beside its row, never inside the row's
// button. With no runtime offered there is no picker.
func TestThePanelPicksARuntimeAndModelAndMarksTheDefault(t *testing.T) {
	var b strings.Builder
	if err := RenderChiefPanelBody(&b, chiefPanelWithSessions([]SessionRuntimeView{
		{Runtime: "claude-code", Label: "Claude Code"}, {Runtime: "gateway", Label: "OpenRouter"}})); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	for _, want := range []string{
		`data-session-picker`,
		`data-new-session="claude-code" hx-post="/agents/105/sessions?surface=chief-panel"`,
		`data-new-session="gateway" hx-post="/agents/105/sessions?surface=chief-panel"`,
		`<input type="hidden" name="runtime" value="claude-code">`,
		`<input type="hidden" name="runtime" value="gateway">`,
		`<option value="opus">opus</option>`,
		`data-combobox-remote="/api/openrouter/models"`,
		`id="chief-session-model-input"`,
		`data-session-runtime="claude-code" class="text-[11px] leading-snug text-muted">Claude Code · opus</span>`,
		`data-session-runtime="gateway" class="text-[11px] leading-snug text-muted">OpenRouter · vendor-x/model-y</span>`,
		`hx-post="/agents/105/sessions/95/default?surface=chief-panel"`,
		`hx-post="/agents/105/sessions/96/default?surface=chief-panel"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the panel lacks %s", want)
		}
	}
	if n := strings.Count(html, "data-session-default"); n != 1 {
		t.Errorf("%d Default badges, want 1", n)
	}
	if strings.Contains(html, "/sessions/92/default") {
		t.Error("the default thread offers to make itself the default")
	}
	// The model the gateway form submits is the combobox's hidden field, and the
	// cluster-default option (an empty value) is not offered: a session cannot be
	// pinned to "whatever the deployment's default is".
	gw := html[strings.Index(html, `data-new-session="gateway"`):]
	gw = gw[:strings.Index(gw, "</form>")]
	if !strings.Contains(gw, `name="model" data-combobox-hidden`) || strings.Contains(gw, `data-value=""`) {
		t.Errorf("the OpenRouter form's model field: %s", gw)
	}
	// "Make default" is never inside a thread's button.
	for _, row := range strings.Split(html, "<button")[1:] {
		btn := row[:strings.Index(row, "</button>")]
		if strings.Contains(btn, "data-chief-thread") && strings.Contains(btn, "data-make-default") {
			t.Error("a Make default control is inside a thread's button")
		}
	}

	b.Reset()
	if err := RenderChiefPanelBody(&b, chiefPanelWithSessions(nil)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "data-session-picker") {
		t.Error("a picker is drawn with no runtime offered")
	}
}
