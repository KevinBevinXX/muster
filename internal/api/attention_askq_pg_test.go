package api

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAHeldQuestionIsOneItemLinkedToItsSessionsView: a session whose
// AskUserQuestion is held (internal/askq, the muster-mirror ask hook) is ONE
// attention item — the question, with its text, linked to the rendered view
// where its card is — never a "session waiting" item beside it, whether the
// window also reports waiting (the hub's overlay) or not. Once answered, a
// window still waiting is a plain session item again; an idle window with
// nothing held is none.
func TestAHeldQuestionIsOneItemLinkedToItsSessionsView(t *testing.T) {
	f := newAttnFixture(t)
	ctx := asUser(1)
	view := "/mirror/view?host=ws&session=main&window=%401"
	key := sessionKey("ws", "main", "@1")
	items := func() []attentionItem {
		all, err := f.srv.attentionItems(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []attentionItem
		for _, it := range all {
			if it.Kind == attentionKindSession || it.Kind == attentionKindQuestion {
				out = append(out, it)
			}
		}
		return out
	}
	qs := []askq.Question{{Question: "Which database?", Header: "DB", Options: []askq.Option{{Label: "postgres"}, {Label: "sqlite"}}},
		{Question: "Migrate now?", Options: []askq.Option{{Label: "yes"}, {Label: "no"}}}}
	e, err := f.srv.Questions().Open(askq.Key{Owner: 1, Source: askSourceMirror, Scope: "ws", Session: focusSID, ToolUseID: "toolu_attn1"},
		qs, askq.NewID())
	if err != nil {
		t.Fatal(err)
	}
	// The window reports waiting (Claude Code does, with its picker up) AND a question is open: one item.
	got := items()
	if len(got) != 1 || got[0].Key != key || got[0].Kind != attentionKindQuestion || got[0].Text != "Which database? (+1 more)" || got[0].Link != view {
		t.Fatalf("held + waiting: %+v", got)
	}
	if body := f.page(t, "/attention/items"); strings.Count(body, `id="`+key+`"`) != 1 || !strings.Contains(body, "Answer in the session") {
		t.Fatalf("the view: %s", body)
	}
	// Held while the host still says idle: the question is still listed, once.
	f.setWindows(mirror.StatusIdle)
	if got := items(); len(got) != 1 || got[0].Kind != attentionKindQuestion {
		t.Fatalf("held + idle: %+v", got)
	}
	// Answered: no question; the idle window is no item at all.
	if _, ok := f.srv.Questions().Resolve(e.Key.Owner, e.Key.Source, e.Key.Scope, e.Key.Session, e.Key.ToolUseID,
		map[string]string{"Which database?": "postgres", "Migrate now?": "yes"}); !ok {
		t.Fatal("not resolved")
	}
	if got := items(); len(got) != 0 {
		t.Fatalf("answered + idle: %+v", got)
	}
	// …and a window still waiting for something else is a session item again.
	f.setWindows(mirror.StatusWaiting)
	if got := items(); len(got) != 1 || got[0].Kind != attentionKindSession {
		t.Fatalf("answered + waiting: %+v", got)
	}
	// Another owner's held question is not the operator's item.
	if _, err := f.srv.Questions().Open(askq.Key{Owner: f.member, Source: askSourceMirror, Scope: "ws", Session: focusSID, ToolUseID: "toolu_attn2"},
		qs, askq.NewID()); err != nil {
		t.Fatal(err)
	}
	if got := items(); len(got) != 1 || got[0].Kind != attentionKindSession {
		t.Fatalf("another owner's question: %+v", got)
	}
}
