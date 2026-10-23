package api

import (
	"context"
	"strconv"

	"time"

	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
)

// ---------------------------------------------------------------------------
// A REPLY DRAFT'S NOTIFICATION, WITHDRAWN HOWEVER IT STOPS WAITING.
//
// A pending draft is an attention item (attention.go) whose notification is
// its `plugin` push (notifyDraftAwaiting, tag plugin-draft-<id>). It stops
// waiting three ways, and each withdraws that notification and moves the
// owner's badge and views:
//   - decided on the Asks page's Reply drafts tab (decideDraft → draftSettled);
//   - superseded by a newer draft for the same message (draftFiled);
//   - expired unseen (expireDraftNotes, on the attention sweep).
// The last two need to know which drafts were notified, so this process
// tracks them (attentionState.drafts) — seeded once per process from the
// store's pending drafts (plugins.PGStore.PendingDrafts), so a draft notified
// before a restart is still withdrawn when it expires.
// ---------------------------------------------------------------------------

// draftNote is one tracked pending draft.
type draftNote struct {
	owner, message int64
	expires        time.Time
}

// pendingDraftLister is the store's system read the seed uses;
// *plugins.PGStore has it, a store without it seeds nothing.
type pendingDraftLister interface {
	PendingDrafts(ctx context.Context) ([]plugins.PendingDraft, error)
}

// draftTag is a draft's notification tag (notifyDraftAwaiting's).
func draftTag(id int64) string { return "plugin-draft-" + strconv.FormatInt(id, 10) }

// clearDraftNote withdraws draft id's notification and moves owner's views.
func (s *Server) clearDraftNote(owner, id int64) {
	s.goNotify(owner, push.EventResolved, RouterNotification{Type: push.TypeResolved, ID: strconv.FormatInt(id, 10), Tag: draftTag(id)}, nil)
	s.attentionChanged(owner, draftKey(id))
}

// draftFiled tracks a just-notified draft and withdraws the drafts it
// superseded (the same message's earlier pending ones).
func (s *Server) draftFiled(owner int64, d plugins.Draft) {
	var gone []int64
	s.attention.mu.Lock()
	if s.attention.drafts == nil {
		s.attention.drafts = map[int64]draftNote{}
	}
	for id, n := range s.attention.drafts {
		if n.message == d.MessageID && id != d.ID {
			gone = append(gone, id)
			delete(s.attention.drafts, id)
		}
	}
	s.attention.drafts[d.ID] = draftNote{owner: owner, message: d.MessageID, expires: d.ExpiresAt}
	s.attention.mu.Unlock()
	for _, id := range gone {
		s.clearDraftNote(owner, id)
	}
	s.attentionChanged(owner, draftKey(d.ID))
}

// draftSettled is a decided draft: untracked, its notification withdrawn.
func (s *Server) draftSettled(owner, id int64) {
	s.attention.mu.Lock()
	delete(s.attention.drafts, id)
	s.attention.mu.Unlock()
	s.clearDraftNote(owner, id)
}

// expireDraftNotes withdraws the notification of every tracked draft past its
// expiry (seeding the tracker from the store on its first run).
func (s *Server) expireDraftNotes(ctx context.Context) {
	if s.ext.Plugins == nil {
		return
	}
	s.attention.mu.Lock()
	seeded := s.attention.draftsSeeded
	s.attention.mu.Unlock()
	if !seeded {
		var seed []plugins.PendingDraft
		if l, ok := s.ext.Plugins.(pendingDraftLister); ok {
			var err error
			if seed, err = l.PendingDrafts(ctx); err != nil {
				s.logger.Printf("attention: read pending drafts: %v", err)
				return
			}
		}
		s.attention.mu.Lock()
		if s.attention.drafts == nil {
			s.attention.drafts = map[int64]draftNote{}
		}
		s.attention.draftsSeeded = true
		for _, d := range seed {
			if _, ok := s.attention.drafts[d.ID]; !ok {
				s.attention.drafts[d.ID] = draftNote{owner: d.OwnerID, message: d.MessageID, expires: d.ExpiresAt}
			}
		}
		s.attention.mu.Unlock()
	}
	now := s.now()
	type due struct{ owner, id int64 }
	var expired []due
	s.attention.mu.Lock()
	for id, n := range s.attention.drafts {
		if !n.expires.After(now) {
			expired = append(expired, due{n.owner, id})
			delete(s.attention.drafts, id)
		}
	}
	s.attention.mu.Unlock()
	for _, d := range expired {
		s.clearDraftNote(d.owner, d.id)
	}
}
