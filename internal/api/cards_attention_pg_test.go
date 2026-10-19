package api

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestTheCardBadgesAreTheQueuesItems: every kind a card counts, on both kinds
// of card, on a real Postgres — and the badges are exactly the queue's items
// that a card stands for, each on one card.
//
//   - agent op: two asks and a reply draft (the draft is NEWEST) — 3, the
//     ask's icon, linking to the newer ask;
//   - agent op2: one ask;
//   - window @1: a held AskUserQuestion and a NEWER ask from its Claude
//     session — 2, the question's icon (more urgent, though older);
//   - window @2 (idle): an ask from its Claude session, found by session id;
//   - window @3: waiting at a prompt;
//   - an ask from a session no window runs: on the queue, on no card.
func TestTheCardBadgesAreTheQueuesItems(t *testing.T) {
	f := newAttnFixture(t)
	ctx := asUser(1)
	f.mirror.mu.Lock()
	f.mirror.windows[1].ClaudeSession = "sess-two"
	f.mirror.windows = append(f.mirror.windows, mirror.Window{Session: "main", ID: "@3", Index: 3, Claude: true, ClaudeSession: "sess-three",
		Status: mirror.StatusWaiting, StatusSince: attnSince, Activity: 1_700_000_000})
	f.mirror.mu.Unlock()

	if _, err := f.srv.Questions().Open(askq.Key{Owner: 1, Source: askSourceMirror, Scope: "ws", Session: focusSID, ToolUseID: "toolu_cards1"},
		[]askq.Question{{Question: "Which database?", Options: []askq.Option{{Label: "postgres"}, {Label: "sqlite"}}}}, askq.NewID()); err != nil {
		t.Fatal(err)
	}
	opOld := f.ask(t, f.opTok, "The op's first question?")
	opNew := f.ask(t, f.opTok, "The op's second question?")
	op2 := f.ask(t, f.op2Tok, "The other agent's question?")
	_, draft := f.pendingDraft(t, "a reply")
	mainAsk := f.mirrorAsk(t, "Asked from window @1?")
	twoAsk := f.askAs(t, attention.Asker{MirrorHost: "ws", MirrorSession: "sess-two", Label: "main:2 @ ws"}, "Asked from window @2?")
	gone := f.askAs(t, attention.Asker{MirrorHost: "ws", MirrorSession: "sess-gone", Label: "gone @ ws"}, "Asked from a session no window runs?")

	items, err := f.srv.attentionItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	x := f.srv.cardAttention(ctx)
	w := func(id string) ui.MirrorRef { return ui.MirrorRef{Host: "ws", Session: "main", Window: id} }
	for _, c := range []struct {
		name string
		got  *ui.CardAttention
		want ui.CardAttention
	}{
		{"agent op", x.agents[f.op.ID], ui.CardAttention{Count: 3, Kind: attentionKindAsk, Href: "/attention#" + attentionAskKey(opNew),
			Keys: []string{draftKey(draft.ID), attentionAskKey(opNew), attentionAskKey(opOld)}}},
		{"agent op2", x.agents[f.op2.ID], ui.CardAttention{Count: 1, Kind: attentionKindAsk, Href: "/attention#" + attentionAskKey(op2),
			Keys: []string{attentionAskKey(op2)}}},
		{"window @1", x.mirrors[w("@1")], ui.CardAttention{Count: 2, Kind: attentionKindQuestion, Href: "/attention#" + sessionKey("ws", "main", "@1"),
			Keys: []string{attentionAskKey(mainAsk.ID), sessionKey("ws", "main", "@1")}}},
		{"window @2", x.mirrors[w("@2")], ui.CardAttention{Count: 1, Kind: attentionKindAsk, Href: "/attention#" + attentionAskKey(twoAsk.ID),
			Keys: []string{attentionAskKey(twoAsk.ID)}}},
		{"window @3", x.mirrors[w("@3")], ui.CardAttention{Count: 1, Kind: attentionKindSession, Href: "/attention#" + sessionKey("ws", "main", "@3"),
			Keys: []string{sessionKey("ws", "main", "@3")}}},
	} {
		if c.got == nil {
			t.Errorf("%s: no badge, want %+v", c.name, c.want)
			continue
		}
		// Keys are compared as a set: two items created in one millisecond tie
		// on Since and fall back to the queue's Key order.
		gotKeys, wantKeys := slices.Sorted(slices.Values(c.got.Keys)), slices.Sorted(slices.Values(c.want.Keys))
		if c.got.Count != c.want.Count || c.got.Kind != c.want.Kind || c.got.Href != c.want.Href || !slices.Equal(gotKeys, wantKeys) {
			t.Errorf("%s: %+v, want %+v", c.name, *c.got, c.want)
		}
	}

	// THE SEAM: the badges' keys are the queue's, each on ONE card, and every
	// queue item a card stands for is on one. Only the session no window runs
	// is on no card.
	var onCards []string
	for _, a := range x.agents {
		onCards = append(onCards, a.Keys...)
	}
	for _, m := range x.mirrors {
		onCards = append(onCards, m.Keys...)
	}
	var queue []string
	for _, it := range items {
		queue = append(queue, it.Key)
	}
	slices.Sort(onCards)
	if len(slices.Compact(slices.Clone(onCards))) != len(onCards) {
		t.Fatalf("an item is on two cards: %v", onCards)
	}
	want := slices.DeleteFunc(slices.Clone(queue), func(k string) bool { return k == attentionAskKey(gone.ID) })
	slices.Sort(want)
	if !slices.Equal(onCards, want) {
		t.Fatalf("the cards' items %v, want the queue's %v less the cardless ask", onCards, want)
	}
	if !slices.Contains(queue, attentionAskKey(gone.ID)) {
		t.Fatal("instrument check: the cardless ask is not on the queue")
	}

	// An ANSWERED ask (#152: listed under "Answered", its delivery queued) is no
	// longer pending: off the badge, though the page still shows it answered.
	if _, err := f.attn.Answer(ctx, opNew, "yes"); err != nil {
		t.Fatal(err)
	}
	after := f.srv.cardAttention(ctx).agents[f.op.ID]
	if after == nil || after.Count != 2 || slices.Contains(after.Keys, attentionAskKey(opNew)) || after.Href != "/attention#"+attentionAskKey(opOld) {
		t.Fatalf("after answering %d, op's badge = %+v, want 2 (the older ask and the draft)", opNew, after)
	}
	if !slices.ContainsFunc(f.srv.recentAnswers(ctx), func(a attention.Ask) bool { return a.ID == opNew }) {
		t.Fatal("instrument check: the answered ask is not in the Answered listing")
	}

	// The list draws them: each card its own badge, a link to the queue item.
	list := f.page(t, "/ui/agents")
	opCard := articleWithID(t, list, "agent-"+strconv.FormatInt(f.op.ID, 10))
	if !strings.Contains(opCard, `href="/attention#`+attentionAskKey(opOld)+`"`) || !strings.Contains(opCard, `data-kind="ask" data-count="2"`) {
		t.Fatalf("op's card does not carry its badge:\n%s", opCard)
	}
	if n := strings.Count(list, "data-attention-badge"); n != 5 {
		t.Fatalf("%d badges on the list, want 5 (op, op2, @1, @2, @3)", n)
	}
}

// articleWithID is the <article id="id"> element of markup.
func articleWithID(t *testing.T, markup, id string) string {
	t.Helper()
	i := strings.Index(markup, `<article id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no card %s in:\n%s", id, markup)
	}
	rest := markup[i:]
	if j := strings.Index(rest, "</article>"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// TestEveryAttentionKindHasABadgeIcon: a kind a card counts with no icon would
// draw an empty badge.
func TestEveryAttentionKindHasABadgeIcon(t *testing.T) {
	for _, k := range attentionUrgency {
		if ic, ok := ui.AttentionKindIcons[k]; !ok || ic.Icon == "" || ic.Words == "" {
			t.Errorf("attention kind %q has no badge icon", k)
		}
	}
	if len(ui.AttentionKindIcons) != len(attentionUrgency) {
		t.Errorf("the icons name %d kinds, the urgency order %d", len(ui.AttentionKindIcons), len(attentionUrgency))
	}
}
