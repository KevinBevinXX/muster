package ui

import (
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// CardAttention is what a card's ATTENTION BADGE shows: how many items of the
// attention queue are this card's (an agent's or a mirrored window's asks, its
// waiting session, its held question, its reply drafts), the most urgent one's
// kind, and where that item is on the queue page.
//
// 🔴 IT IS NOT DERIVED HERE, AND IT MUST NOT BE. internal/api composes it from
// attentionItems — the ONE read the queue page, the sidebar badge and the
// chief's attention_list make — so a card and the queue cannot disagree about
// what is waiting.
type CardAttention struct {
	// Count is the card's items; the badge shows only for Count > 0.
	Count int
	// Kind is the most urgent item's kind ("session", "question", "ask",
	// "plugin": internal/api's attention kinds), which picks the icon.
	Kind string
	// Href opens that item on the queue page (/attention#<its key>).
	Href string
	// Keys are the card's items' keys, newest first: what a test compares
	// with the queue's.
	Keys []string
}

// AttentionKindIcons is each attention kind's badge icon and the words a screen
// reader hears for it. internal/api's TestEveryAttentionKindHasABadgeIcon pins
// that every kind a card can count is here.
var AttentionKindIcons = map[string]struct{ Icon, Words string }{
	"session":  {"🔒", "a session waiting at a prompt"},
	"question": {"💬", "a session asking a question"},
	"ask":      {"❓", "an agent's question"},
	"plugin":   {"✉️", "a reply draft to approve"},
}

// cardAttentionBadge is the card's attention badge: the count and the most
// urgent kind's icon, a link to that item on the queue page. It sits in the
// card's CONTROL CLUSTER, outside the nav anchor, so a tap opens the queue and
// never the card; hx-boost="false" because /attention is a standalone
// document (and the list's inherited hx-target would otherwise swallow it).
// Nothing for no items.
func cardAttentionBadge(a *CardAttention) g.Node {
	if a == nil || a.Count <= 0 {
		return nil
	}
	ic := AttentionKindIcons[a.Kind]
	n := strconv.Itoa(a.Count)
	label := n + " waiting on you; the most urgent is " + ic.Words + ". Open it in the attention queue"
	return A(
		Href(a.Href),
		hx("hx-boost", "false"),
		g.Attr("data-attention-badge", ""),
		g.Attr("data-kind", a.Kind),
		g.Attr("data-count", n),
		g.Attr("aria-label", label),
		g.Attr("title", label),
		Class("press inline-flex h-11 min-w-[44px] items-center justify-center gap-1 rounded-xl px-1.5 text-sm transition hover:bg-s2"),
		Span(g.Attr("aria-hidden", "true"), g.Text(ic.Icon)),
		Span(Class("inline-flex min-w-[1.5rem] items-center justify-center rounded-full bg-accent px-1.5 text-xs font-semibold text-on-accent"), g.Text(n)),
	)
}

// cardReserve is the right padding a card's title rows keep clear of its
// control cluster (cardControls): two 44 px controls, or — with an attention
// badge in the cluster too — that and the badge (up to ~60 px), so a long title
// wraps before it reaches either.
func cardReserve(a *CardAttention) string {
	if a != nil && a.Count > 0 {
		return "pr-44"
	}
	return "pr-28"
}
