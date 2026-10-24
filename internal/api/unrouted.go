package api

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/fence"
	"github.com/ZacxDev/muster/internal/plugins"
)

// A PLUGIN MESSAGE THE CHIEF DID NOT ACT ON BECOMES AN ASK, raised by muster
// (a system asker) on the attention sweep: the rules, and why, are in
// docs/plugins.md#a-message-the-chief-does-not-act-on; the SQL is
// internal/plugins unrouted.go.

const pluginMessageSettleToolName = "plugin_message_settle"

// unroutedNoAction is the ask's one offered answer.
const unroutedNoAction = "No action needed"

// unroutedSweepBatch bounds how many asks one sweep raises, and closes.
const unroutedSweepBatch = 50

// unroutedPreviewRunes bounds the message body the ask quotes.
const unroutedPreviewRunes = 600

func pluginMessageSettleToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: pluginMessageSettleToolName,
		Description: "Record that a plugin message delivered to you needs nothing more — a notice, a status update, a " +
			"newsletter, or one dealt with some other way — and why. A repliable agent-mail message you neither route to a " +
			"session, nor answer with reply_draft, nor cite in task_create_gated, nor settle here is put in front of your owner as an ask " +
			strconv.Itoa(int(plugins.UnroutedAskAfter/time.Minute)) + " minutes after it reached you; settling it after " +
			"that withdraws the ask. Settling sends nothing to anyone. Your reason is recorded with the message and shown " +
			"to your owner. A message already settled keeps its first reason.",
		Parameters: objSchema(map[string]any{
			"message_id": map[string]any{"type": "integer", "description": "The plugin message (its message_id)."},
			"reason":     strProp("Why it needs nothing more, in a sentence. At most 2000 characters."),
		}, "message_id", "reason")}
}

func (s *Server) toolPluginMessageSettle(ctx context.Context, a agents.Agent, arguments string) string {
	if s.ext.Plugins == nil {
		return toolErr("%s is not available: plugins are not enabled on this deployment", pluginMessageSettleToolName)
	}
	var in struct {
		MessageID int64  `json:"message_id"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil || in.MessageID <= 0 {
		return toolErr("arguments are {message_id, reason}")
	}
	m, fresh, err := s.ext.Plugins.SettleMessage(ctx, a.ID, agentOwnerID(a), in.MessageID, in.Reason)
	switch {
	case err == nil:
	case errors.Is(err, plugins.ErrNotFound):
		return toolErr("message_id %d is not a plugin message delivered to you", in.MessageID)
	case errors.Is(err, plugins.ErrInvalid):
		return toolErr("%s", strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": "))
	default:
		s.logger.Printf("plugins: agent %d settle message %d: %v", a.ID, in.MessageID, err)
		return toolErr("the message could not be settled")
	}
	out := map[string]any{"message_id": m.ID, "settled": true, "reason": *m.SettleReason,
		"next": "Nothing more to do: no ask will be raised for it, and one already raised is withdrawn."}
	if !fresh {
		out["already_settled"] = true
		out["next"] = "It was already settled; its first reason is kept."
	}
	return toolJSON(out)
}

// unroutedNowTime is the unrouted-message sweep's clock (a test's, else the wall
// clock).
func (s *Server) unroutedNowTime() time.Time {
	if s.unroutedNow != nil {
		return s.unroutedNow()
	}
	return time.Now()
}

// sweepUnroutedMessages is the attention sweep's unrouted-message step: raise
// an ask for every message now due one (plugins.UnroutedDue), then close every
// open one whose message has since been dispositioned. Nothing without the
// plugin store.
func (s *Server) sweepUnroutedMessages(ctx context.Context) {
	if s.ext.Plugins == nil {
		return
	}
	asOf := s.unroutedNowTime()
	due, err := s.ext.Plugins.UnroutedDue(ctx, asOf, unroutedSweepBatch)
	if err != nil {
		s.logger.Printf("plugins: unrouted messages: %v", err)
	}
	capped := 0
	for _, m := range due {
		if errors.Is(s.raiseUnroutedAsk(ctx, m), attention.ErrTooMany) {
			capped++
		}
	}
	if capped > 0 {
		// The owner's open asks are at MaxOpenPerOwner; each is retried on the
		// next sweep. One line per sweep, not one per message.
		s.logger.Printf("plugins: %d unrouted message(s) wait for an ask: their owner's open asks are at the cap (%d)", capped, attention.MaxOpenPerOwner)
	}
	closing, err := s.ext.Plugins.UnroutedAsksToClose(ctx, unroutedSweepBatch)
	if err != nil {
		s.logger.Printf("plugins: unrouted asks to close: %v", err)
	}
	for _, u := range closing {
		if _, err := s.cancelAsk(s.attentionPrincipal(ctx, u.OwnerID), u.AskID); err != nil && !errors.Is(err, attention.ErrNotOpen) {
			s.logger.Printf("plugins: close ask %d (message %d was acted on): %v", u.AskID, u.MessageID, err)
		}
	}
}

// raiseUnroutedAsk raises the ask for message m, as its owner, and tells the
// owner's devices; ErrTooMany when the owner's open asks are at the cap. A
// disposition racing it is caught by the next sweep's close;
// a second sweep racing it, by the unique link.
func (s *Server) raiseUnroutedAsk(ctx context.Context, m plugins.Message) error {
	octx := s.attentionPrincipal(ctx, m.OwnerID)
	ask, err := s.ext.Attention.Create(octx, attention.New{
		Asker:           attention.Asker{System: true, Label: attention.SystemAskerLabel},
		Question:        s.unroutedQuestion(octx, m),
		Options:         []string{unroutedNoAction},
		PluginMessageID: m.ID,
	})
	var pg *pgconn.PgError
	switch {
	case errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "attention_asks_one_per_plugin_message":
		return nil // another sweep raised it first
	case errors.Is(err, attention.ErrTooMany):
		return err // the caller logs once per sweep
	case err != nil:
		s.logger.Printf("plugins: raise an ask for message %d: %v", m.ID, err)
		return err
	}
	s.attentionNotify(ask.Owner, askItem(ask))
	s.attentionChanged(ask.Owner, attentionAskKey(ask.ID))
	return nil
}

// unroutedQuestion is the ask for message m: muster's sentence, then who sent
// it (with the UNVERIFIED mark unless the plugin verified the sender), its
// subject, its plugin and id, and a bounded preview of
// its body — every outside field sanitised.
func (s *Server) unroutedQuestion(ctx context.Context, m plugins.Message) string {
	label := "a plugin"
	if p, err := s.ext.Plugins.Plugin(ctx, m.PluginID); err == nil {
		label = fence.Sanitize(p.Label)
	}
	subject := fence.Sanitize(m.Subject)
	if strings.TrimSpace(subject) == "" {
		subject = "(none)"
	}
	q := "A message from " + label + " has not been acted on: " + strconv.Itoa(int(plugins.UnroutedAskAfter/time.Minute)) +
		" minutes after it reached your chief, it is not routed to a session, has no reply draft, and was not settled.\n\n" +
		"From: " + plugins.SenderText(m) + "\n" +
		"Subject: " + subject + "\n" +
		"Message: " + strconv.FormatInt(m.ID, 10) + "\n\n" +
		fence.Sanitize(fence.Cut(m.Body, unroutedPreviewRunes)) + "\n\n" +
		"Your answer settles the message, with your answer as the reason; it sends nothing to the sender. " +
		"To reply, ask your chief to draft one."
	return fence.Cut(q, attention.MaxQuestionRunes)
}

// settleAnsweredMessage settles the message ask a — just answered by the
// owner — was raised for, if any, with the answer as the reason
// (plugins.SettleByAsk; nothing for any other ask).
func (s *Server) settleAnsweredMessage(ctx context.Context, a attention.Ask) {
	if s.ext.Plugins == nil {
		return
	}
	if _, err := s.ext.Plugins.SettleByAsk(ctx, a.ID, a.Answer); err != nil {
		s.logger.Printf("plugins: settle the message ask %d was raised for: %v", a.ID, err)
	}
}
