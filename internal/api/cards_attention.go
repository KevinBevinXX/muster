package api

import (
	"context"
	"slices"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE CARDS' ATTENTION BADGES: each Agents-list card — a dispatched agent or a
// mirrored Claude window — shows how many items of the attention queue are its
// own, and the most urgent one's icon, linking to that item on the queue page.
//
// 🔴 ONE SOURCE. The badges are built from attentionItems — the read the queue
// page, the sidebar badge and attention_list all make — and from each item's
// own card address (attentionItem.card, set where the item is built). There is
// no second "is this card waiting" predicate to drift from the queue's:
// TestTheCardBadgesAreTheQueuesItems compares the two.
//
// An item no card stands for — a routed task, a draft whose agent was deleted,
// an asking session whose window is not listed — is on the queue and on no
// badge.

// attentionUrgency orders the kinds a card counts, MOST URGENT FIRST: a
// session stopped at a permission prompt (its turn is blocked mid-work), a held
// AskUserQuestion (blocked, asking), an agent's explicit question, then a reply
// draft (nothing is blocked; a message waits). The badge's icon and link are
// the most urgent kind's newest item.
var attentionUrgency = []string{attentionKindSession, attentionKindQuestion, attentionKindAsk, attentionKindPlugin}

// cardAttentionIndex is every card's badge, by agent id and by mirrored
// address.
type cardAttentionIndex struct {
	agents  map[int64]*ui.CardAttention
	mirrors map[ui.MirrorRef]*ui.CardAttention
}

// add counts it on the card at.
func (x *cardAttentionIndex) add(at *ui.CardAttention, it attentionItem) {
	at.Count++
	at.Keys = append(at.Keys, it.Key)
	if !slices.Contains(at.Kinds, it.Kind) {
		at.Kinds = append(at.Kinds, it.Kind)
	}
	cur := slices.Index(attentionUrgency, at.Kind)
	if at.Kind == "" || slices.Index(attentionUrgency, it.Kind) < cur {
		at.Kind, at.Href = it.Kind, attentionPath+"#"+it.Key
	}
}

// cardAttention is ctx's principal's badges: attentionItems (NEWEST first, so
// a kind's first item is its newest), each on the card its address names. An
// asking mirrored session is resolved to the window running its Claude session
// — in the active pane or another pane of it — on its host's list, offline or
// not (its card is drawn either way). Empty when the queue is not served:
// the badge would link to a page that 404s.
func (s *Server) cardAttention(ctx context.Context) cardAttentionIndex {
	x := cardAttentionIndex{agents: map[int64]*ui.CardAttention{}, mirrors: map[ui.MirrorRef]*ui.CardAttention{}}
	if s.ext.Attention == nil {
		return x
	}
	items, err := s.attentionItems(ctx)
	if err != nil {
		s.logger.Printf("attention: cards: %v", err)
	}
	var hosts []mirror.HostView
	if s.ext.Mirror != nil {
		hosts = s.ext.Mirror.Hosts(ctx)
	}
	for _, it := range items {
		if !slices.Contains(attentionUrgency, it.Kind) {
			continue
		}
		c := it.card
		switch {
		case c.agentID > 0:
			at := x.agents[c.agentID]
			if at == nil {
				at = &ui.CardAttention{}
				x.agents[c.agentID] = at
			}
			x.add(at, it)
		case c.claudeSession != "":
			ref, ok := claudeSessionWindow(hosts, c.ref.Host, c.claudeSession)
			if !ok {
				continue
			}
			c.ref = ref
			fallthrough
		case c.ref.Host != "":
			at := x.mirrors[c.ref]
			if at == nil {
				at = &ui.CardAttention{}
				x.mirrors[c.ref] = at
			}
			x.add(at, it)
		}
	}
	return x
}

// claudeSessionWindow is the address of host's window running Claude session
// sid, in its active pane or a split (the MCP door's rule, mirrorSessionOf).
func claudeSessionWindow(hosts []mirror.HostView, host, sid string) (ui.MirrorRef, bool) {
	for _, h := range hosts {
		if h.Host != host {
			continue
		}
		for _, w := range h.Windows {
			if (w.Claude && w.ClaudeSession == sid) || slices.Contains(w.OtherClaude, sid) {
				return ui.MirrorRef{Host: h.Host, Session: w.Session, Window: w.ID}, true
			}
		}
	}
	return ui.MirrorRef{}, false
}

// withCardAttention sets every card's badge (in place).
func (s *Server) withCardAttention(ctx context.Context, cards []ui.AgentCardView, mirrors []ui.MirrorCardView) {
	if len(cards) == 0 && len(mirrors) == 0 {
		return
	}
	x := s.cardAttention(ctx)
	for i := range cards {
		cards[i].Attention = x.agents[cards[i].ID]
	}
	for i := range mirrors {
		m := mirrors[i]
		mirrors[i].Attention = x.mirrors[ui.MirrorRef{Host: m.Host, Session: m.Session, Window: m.Window}]
	}
}
