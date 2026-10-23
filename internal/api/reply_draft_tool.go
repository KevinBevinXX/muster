package api

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
)

// THE CHIEF'S WAY TO ANSWER A PLUGIN MESSAGE: reply_draft (plugin interface, PR C).
//
// The chief never sends anything. It files a DRAFT; the owner reads the exact
// text on /plugins and approves it (with a fresh sign-in) or rejects it; only an
// approved draft is ever handed to the plugin, which sends it upstream.
//
// 🔴 EVERY RULE IS THE STORE'S (plugins.PGStore.FileDraft), so it holds whatever
// calls it: the message must have been delivered to THIS agent of THIS owner, it
// must not be marked no_reply (migration 0016 refuses that draft on INSERT too),
// the text passes plugins.ScreenDraft, and one agent may have at most
// plugins.MaxPendingDraftsPerAgent pending. This file maps refusals to words.

const replyDraftToolName = "reply_draft"

func replyDraftToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: replyDraftToolName,
		Description: "Draft a reply to a plugin message. NOTHING is sent: your owner reads the exact text and " +
			"approves or rejects it, and only an approved draft is sent, once, by the plugin. A new draft for the " +
			"same message replaces your pending one. Messages marked as not replyable are refused. " +
			"Write the reply in your own words; do not include addresses, links, credentials or infrastructure names. " +
			"At most " + strconv.Itoa(plugins.MaxPendingDraftsPerAgent) + " drafts may await your owner at once.",
		Parameters: objSchema(map[string]any{
			"message_id": map[string]any{"type": "integer", "description": "The plugin message you are answering (its message_id)."},
			"body":       strProp("The reply, exactly as it should be sent. At most 16 KiB."),
		}, "message_id", "body")}
}

func (s *Server) toolReplyDraft(ctx context.Context, a agents.Agent, arguments string) string {
	if s.ext.Plugins == nil {
		return toolErr("%s is not available: plugins are not enabled on this deployment", replyDraftToolName)
	}
	var in struct {
		MessageID int64  `json:"message_id"`
		Body      string `json:"body"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil || in.MessageID <= 0 {
		return toolErr("arguments are {message_id, body}")
	}
	d, superseded, err := s.ext.Plugins.FileDraft(ctx, a.ID, agentOwnerID(a), a.Name, in.MessageID, in.Body)
	switch {
	case err == nil:
	case errors.Is(err, plugins.ErrNotFound):
		return toolErr("message_id %d is not a plugin message delivered to you", in.MessageID)
	case errors.Is(err, plugins.ErrNoReply):
		return toolErr("message_id %d cannot be replied to: its plugin marked it as not replyable. Do not draft a reply to it", in.MessageID)
	case errors.Is(err, plugins.ErrInvalid):
		return toolErr("%s", strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": "))
	case errors.Is(err, plugins.ErrReplyInFlight):
		return toolErr("a reply to message_id %d is already approved and on its way; wait until it is sent before drafting another", in.MessageID)
	case errors.Is(err, plugins.ErrTooManyPendingDrafts):
		return toolErr("%s", strings.TrimPrefix(err.Error(), "plugins: "))
	default:
		s.logger.Printf("plugins: agent %d reply draft: %v", a.ID, err)
		return toolErr("the draft could not be filed")
	}
	// The message, for the notification's excerpt; without it the push says
	// only muster's own words.
	m, merr := s.ext.Plugins.Message(ctx, d.MessageID)
	if merr != nil {
		m = plugins.Message{}
	}
	s.notifyDraftAwaiting(ctx, agentOwnerID(a), d, m)
	// It is also an item of the owner's attention queue; it supersedes the
	// message's earlier pending draft, whose notification is withdrawn.
	s.draftFiled(agentOwnerID(a), d)
	return toolJSON(map[string]any{
		"draft_id": d.ID, "status": string(d.Status), "superseded": superseded,
		"next": "Your owner must approve this draft in muster; nothing is sent until they do. Do not file it again.",
	})
}

// draftCardAnchor is a draft card's fragment on the Asks page (draftLink).
func draftCardAnchor(id int64) string { return "draft-" + strconv.FormatInt(id, 10) }

// notifyDraftAwaiting pushes the `plugin` event to the draft's owner.
//
// 🔴 WHO AND WHERE, NEVER WHAT (operator decision "Summary only"): the title is
// muster's; the body is "Reply draft: <sender's name> (<plugin label>)"
// (draftPushBody) — no word of the message or of the draft, which are an
// outside party's and the chief's text on a lock screen with no fence. The
// queue page and /plugins show them, behind the sign-in. Without the message
// (m.ID 0) it is muster's fixed sentence.
func (s *Server) notifyDraftAwaiting(ctx context.Context, owner int64, d plugins.Draft, m plugins.Message) {
	id := strconv.FormatInt(d.ID, 10)
	label := ""
	if m.ID != 0 {
		if p, err := s.ext.Plugins.Plugin(ctx, m.PluginID); err == nil {
			label = p.Label
		}
	}
	s.goNotify(owner, push.EventPlugin, RouterNotification{
		Type:  "plugin",
		ID:    id,
		Title: DraftPushTitle,
		Body:  draftPushBody(m, label),
		Tag:   "plugin-draft-" + id,
		Data:  map[string]string{"url": draftLink(d.ID)},
	}, nil)
}

// draftPushBody is a draft notification's body: "Reply draft: <sender's
// display name> (<plugin label>)". The name is shown only when the plugin
// VERIFIED the sender (Message.SenderVerified); otherwise it is what the sender
// claims to be, and the body says "unverified sender" instead. DraftPushBody
// without the message.
func draftPushBody(m plugins.Message, pluginLabel string) string {
	if m.ID == 0 {
		return DraftPushBody
	}
	who := unverifiedSender
	if m.SenderVerified {
		if name := senderDisplayName(m.Sender); name != "" {
			who = name
		}
	}
	body := "Reply draft: " + who
	if l := mirror.OneLine(pluginLabel, plugins.MaxLabelRunes); l != "" {
		body += " (" + l + ")"
	}
	return truncate(body, attentionPushBodyMax)
}

// senderDisplayName is a sender as a name: "Ada" of "Ada <ada@example.test>",
// else the sender itself — one line, at most draftPushNameMax runes.
func senderDisplayName(sender string) string {
	if i := strings.Index(sender, "<"); i > 0 {
		if name := strings.Trim(strings.TrimSpace(sender[:i]), `"'`); name != "" {
			sender = name
		}
	}
	return mirror.OneLine(sender, draftPushNameMax)
}

// unverifiedSender is how a one-line surface names a sender the plugin did not
// verify: the draft push says it INSTEAD of the name, the attention item
// (draftInboundText) AFTER it, as unverifiedSenderNote. The chief's turn and a
// routed paste carry the longer plugins.UnverifiedSenderMark.
const unverifiedSender = "unverified sender"

// unverifiedSenderNote follows an unverified sender's name on a one-line
// surface.
const unverifiedSenderNote = "(" + unverifiedSender + ")"

// draftPushNameMax bounds the sender's name in a draft notification.
const draftPushNameMax = 60

// The `plugin` push's title (muster's), and its body when the message cannot
// be read.
const (
	DraftPushTitle = "A reply draft awaits your approval"
	DraftPushBody  = "Your chief drafted a reply to a plugin message. Nothing is sent until you approve it."
)
