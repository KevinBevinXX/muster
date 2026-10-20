package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Start-up recovery, run after EVERY start of the gateway and before it is
// given a turn (supervise.go). docs/design/hermes.md, "Restart and resume".
//
// In this order:
//
//  1. An intent posted under a DIFFERENT API key (the key file was lost or
//     replaced) is resolved from its session's messages, read on the session
//     it was posted to. 🔴 IT IS NEVER RE-POSTED: runs and idempotency keys are
//     scoped by the key, so under a new key a re-post admits a SECOND run.
//  2. Every session whose runs were posted under a different key is RETIRED:
//     mapped to a fresh Hermes session. 🔴 RESOLVE FIRST, THEN RETIRE, so step
//     1 reads the session the intent went to, not the fresh one. Under the new
//     key hermesd cannot see whether an old run is `unknown` and blocking its
//     session (GET and resolve-unknown answer only the owner, 404 otherwise),
//     so it cannot retire only the blocked ones; a session that might be
//     blocked for ever is worse than one that loses its history.
//  3. For every other session: its last run, when Hermes reports it `unknown`,
//     is resolved (POST /v1/runs/{id}/resolve-unknown, outcome interrupted);
//     an intent with a run id is followed (or closed, when it has ended); an
//     intent still `posting` — a crash between the POST and recording its run
//     id — is RE-POSTED with the SAME idempotency key and the SAME bytes, which
//     Hermes answers with the run it already admitted (or admits it, when it
//     never was). Never past the key retention (intentRetention): there a
//     replay could start a second run, so it is resolved from the messages.
//     A 409 idempotency_key_conflict is "may have run", and the intent closes.
//
// A run nobody is waiting for any more ends with a NOTICE on its session, said
// at the start of the next reply there.

// intentRetention is how long Hermes keeps an idempotency key (24 h). An
// intent older than this is never re-posted.
const intentRetention = 24 * time.Hour

const retiredNotice = "The previous Hermes session could not be recovered after the agent's API key changed; " +
	"this turn starts a new session without its history."

func (s *server) recover(ctx context.Context) error {
	// 1. Intents under another key: resolved from the messages, never re-posted.
	for _, key := range s.store.keys() {
		sess, ok := s.store.get(key)
		if !ok || sess.Intent == nil || sess.Intent.KeyFP == s.keyFP {
			continue
		}
		notice, err := s.resolveFromMessages(ctx, sess)
		if err != nil {
			return err
		}
		s.logf("hermesd: recovery: an intent on session %s was posted under another API key; not re-posted (%s)",
			sess.HermesID, firstLine(notice))
		if err := s.store.closeIntent(key, sess.Intent.IdempotencyKey, notice); err != nil {
			return err
		}
	}
	// 2. Sessions posted under another key are retired.
	retired := 0
	for _, key := range s.store.keys() {
		sess, ok := s.store.get(key)
		if !ok || sess.KeyFP == s.keyFP {
			continue
		}
		if err := s.store.retire(key, s.keyFP, retiredNotice); err != nil {
			return err
		}
		retired++
		s.logf("hermesd: recovery: retired session %s (posted under API key %s, now %s)", sess.HermesID, sess.KeyFP, s.keyFP)
	}
	if retired > 0 {
		s.logf("hermesd: recovery: %d session(s) retired after an API key change", retired)
	}
	// 3. This key's sessions: unknown runs resolved, intents settled or replayed.
	for _, key := range s.store.keys() {
		if err := s.recoverSession(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) recoverSession(ctx context.Context, key string) error {
	sess, ok := s.store.get(key)
	if !ok {
		return nil
	}
	if sess.LastRunID != "" {
		if _, err := s.resolveIfUnknown(ctx, sess.LastRunID); err != nil && !isAPIError(err, http.StatusNotFound, "") {
			return fmt.Errorf("session %s: last run %s: %w", sess.HermesID, sess.LastRunID, err)
		}
	}
	in := sess.Intent
	if in == nil {
		return nil
	}
	live := s.isLive(key)
	if in.RunID != "" {
		st, err := s.resolveIfUnknown(ctx, in.RunID)
		switch {
		case isAPIError(err, http.StatusNotFound, ""):
			if live {
				return nil
			}
			return s.store.closeIntent(key, in.IdempotencyKey, "Your previous message's run ("+in.RunID+") is no longer known to Hermes; "+mayHaveRunText+".")
		case err != nil:
			return fmt.Errorf("session %s: run %s: %w", sess.HermesID, in.RunID, err)
		case live:
			return nil // the turn serving it closes it
		case terminalStatus(st.Status):
			return s.store.closeIntent(key, in.IdempotencyKey, outcomeNotice(in.RunID, st))
		}
		go s.watch(s.watchContext(), key, in.IdempotencyKey, in.RunID)
		return nil
	}
	if live {
		return nil // the turn serving it is still posting; it replays itself
	}
	if s.now().Sub(in.At) > intentRetention {
		notice, err := s.resolveFromMessages(ctx, sess)
		if err != nil {
			return err
		}
		s.logf("hermesd: recovery: an intent on session %s is older than %s; not re-posted", sess.HermesID, intentRetention)
		return s.store.closeIntent(key, in.IdempotencyKey, notice)
	}
	return s.replay(ctx, key, *in)
}

// replay re-posts an intent exactly as it was first posted.
func (s *server) replay(ctx context.Context, key string, in intent) error {
	runID, replayed, err := s.api.postRun(ctx, in.IdempotencyKey, in.Body)
	switch {
	case isAPIError(err, http.StatusConflict, "idempotency_key_conflict"):
		return s.store.closeIntent(key, in.IdempotencyKey,
			"Your previous message "+mayHaveRunText+" (Hermes says its idempotency key was used with a different request).")
	case err != nil:
		var ae *apiError
		if errors.As(err, &ae) && ae.Status < 500 && ae.Status != http.StatusTooManyRequests {
			// Refused outright: it never ran.
			return s.store.closeIntent(key, in.IdempotencyKey, "Your previous message was not run: Hermes refused it ("+ae.Error()+").")
		}
		return fmt.Errorf("replay the intent on %s: %w", key, err)
	}
	s.logf("hermesd: recovery: re-posted an unrecorded intent as run %s (replayed=%t)", runID, replayed)
	if err := s.store.setRunID(key, in.IdempotencyKey, runID); err != nil {
		return err
	}
	go s.watch(s.watchContext(), key, in.IdempotencyKey, runID)
	return nil
}

// resolveFromMessages decides what to say about an intent that cannot be
// re-posted, from its session's messages: a user message carrying the intent's
// input means it reached Hermes.
func (s *server) resolveFromMessages(ctx context.Context, sess session) (string, error) {
	var req runRequest
	_ = json.Unmarshal(sess.Intent.Body, &req)
	msgs, err := s.api.sessionMessages(ctx, sess.HermesID)
	if err != nil && !isAPIError(err, http.StatusNotFound, "") {
		return "", fmt.Errorf("session %s: read its messages: %w", sess.HermesID, err)
	}
	want := strings.TrimSpace(req.Input)
	for _, m := range msgs {
		if m.Role == "user" && want != "" && strings.Contains(messageText(m.Content), want) {
			return "Your previous message reached Hermes before hermesd restarted, but its reply was not delivered; " +
				"it may have run in part. Check the work before sending it again.", nil
		}
	}
	return "Your previous message " + mayHaveRunText + ": hermesd restarted before it could confirm it.", nil
}

// messageText is a message's content as text: a string, or the text parts of
// a list.
func messageText(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, p := range v {
			if m, ok := p.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}

// outcomeNotice says how a run nobody was waiting for ended.
func outcomeNotice(runID string, st runStatus) string {
	switch st.Status {
	case "completed":
		if st.Output == "" {
			return "Your previous message's run (" + runID + ") completed while hermesd could not deliver it, with no answer text."
		}
		return "Your previous message's run (" + runID + ") completed while hermesd could not deliver it. Its reply:\n\n" + st.Output
	case "failed":
		return "Your previous message's run (" + runID + ") failed: " + st.Error
	default:
		return "Your previous message's run (" + runID + ") was interrupted (the Hermes gateway restarted) and did not finish."
	}
}

// watch follows a run nobody is streaming until it ends, then closes its
// intent with a notice. It stops with the gateway's epoch: the next start's
// recovery picks the intent up again.
func (s *server) watch(ctx context.Context, key, idem, runID string) {
	sess, _ := s.store.get(key)
	ap := newApprovals(s, runID, sess.HermesID)
	defer ap.stop()
	st, err := s.pollRun(ctx, runID, ap)
	if err != nil {
		return
	}
	if err := s.store.closeIntent(key, idem, outcomeNotice(runID, st)); err != nil {
		s.logf("hermesd: close the intent of %s: %v", runID, err)
	}
}

// watchIntent replays an intent whose POST could not be confirmed until it is
// admitted (then watches its run) or refused.
func (s *server) watchIntent(ctx context.Context, key, idem string) {
	for {
		sess, ok := s.store.get(key)
		if !ok || sess.Intent == nil || sess.Intent.IdempotencyKey != idem || sess.Intent.RunID != "" {
			return
		}
		if s.gatewayState().State == stReady {
			if err := s.replay(ctx, key, *sess.Intent); err == nil {
				return
			}
		}
		if !sleepCtx(ctx, s.watchPoll) {
			return
		}
	}
}

// watchContext is the context a watcher runs under: the current gateway's.
func (s *server) watchContext() context.Context {
	if s.sup == nil {
		return context.Background()
	}
	return s.sup.currentEpoch()
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}
