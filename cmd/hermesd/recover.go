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
//     1 reads the session the intent went to, not the fresh one.
//  3. For every other session: its last run, when Hermes reports it `unknown`,
//     is resolved (resolve-unknown, outcome interrupted); an intent is settled
//     (settleIntent): one whose run has ended is closed; one still `posting` —
//     a crash between the POST and recording its run id — is RE-POSTED with
//     the SAME idempotency key and the SAME bytes, which Hermes answers with
//     the run it already admitted (or admits it, when it never was). Never past
//     the key retention (intentRetention): there a replay could start a second
//     run, so it is resolved from the messages instead. A 409
//     idempotency_key_conflict is "may have run", and the intent closes.
//
// 🔴 NOTHING IS FOLLOWED IN THE BACKGROUND. A run recovery finds still going (a
// replayed intent's) keeps its intent open, so its session answers busy; the
// session's next turn settles it again (settleIntent, the same rules), and a
// run that has ended then closes. Such a run's approvals are not relayed — no
// person is waiting on that turn — so Hermes denies them at its own
// approvals.timeout. (The earlier background watcher re-relayed approvals with
// a fresh window and re-posted with no retention check; both are gone with it.)
//
// 🔴 RECOVERY IS BOUNDED, AND A BROKEN API IS NEVER "FIXED" BY FORGETTING.
// Every call has a deadline (recoveryCallTimeout). Only a session with an open
// intent needs recovery at all: one with none (its last run ended when its
// intent closed) makes no call, so a flaky API cannot touch it. A failure that
// may pass — transport, timeout, 5xx, 429, a store write — is retried, never
// retired: when it persists, recovery as a whole stays unfinished and, past
// recoveryStuckAfter, /healthz fails (supervise.go) so the liveness probe
// restarts the pod. So is a 401 or 403: it refuses the API KEY, every session
// alike, and retiring sessions over it would only make a broken API report
// healthy. Only a DEFINITE refusal of that session's recovery (any other 4xx)
// that has persisted, with no other outcome between, for retireAfter RETIRES
// the session, with the reason — so one poisoned session cannot wedge the pod
// for ever.
//
// A recovery decision about a turn nobody waits for any more (an old-key
// intent, an intent past retention, a key conflict, a retired session) is a
// NOTICE on its session, said at the start of the next reply there.

// intentRetention is how long Hermes keeps an idempotency key (24 h). An
// intent older than this is never re-posted.
const intentRetention = 24 * time.Hour

// recoveryCallTimeout bounds one recovery call to Hermes's API (a variable so a
// test can shorten it).
var recoveryCallTimeout = 15 * time.Second

// retireAfter is how long a session's recovery must have been DEFINITELY
// refused, continuously, before the session is retired. recoveryStuckAfter
// (supervise.go) outlasts it by more than the liveness probe's 90 s, so a
// session refused from the start is retired before the pod is restarted for it.
const retireAfter = 5 * time.Minute

const retiredNotice = "The previous Hermes session could not be recovered after the agent's API key changed; " +
	"this turn starts a new session without its history."

func (s *server) recover(ctx context.Context) error {
	var failed []string
	// 1. Intents under another key: resolved from the messages, never re-posted.
	for _, key := range s.store.keys() {
		sess, ok := s.store.get(key)
		if !ok || sess.Intent == nil || sess.Intent.KeyFP == s.keyFP {
			continue
		}
		notice, err := s.resolveFromMessages(ctx, sess)
		if err == nil {
			s.logf("hermesd: recovery: an intent on session %s was posted under another API key; not re-posted (%s)",
				sess.HermesID, firstLine(notice))
			err = s.store.closeIntent(key, sess.Intent.IdempotencyKey, notice)
		}
		if err != nil {
			failed = append(failed, s.sessionFailed(key, err))
		} else {
			s.clearRefused(key)
		}
	}
	// 2. Sessions posted under another key are retired.
	retired := 0
	for _, key := range s.store.keys() {
		sess, ok := s.store.get(key)
		if !ok || sess.KeyFP == s.keyFP || sess.Intent != nil {
			continue // an old-key intent still open failed step 1, and is retried
		}
		if err := s.store.retire(key, s.keyFP, retiredNotice); err != nil {
			failed = append(failed, s.sessionFailed(key, err))
			continue
		}
		retired++
		s.logf("hermesd: recovery: retired session %s (posted under API key %s, now %s)", sess.HermesID, sess.KeyFP, s.keyFP)
	}
	if retired > 0 {
		s.logf("hermesd: recovery: %d session(s) retired after an API key change", retired)
	}
	// 3. This key's sessions: unknown runs resolved, intents settled.
	for _, key := range s.store.keys() {
		if sess, ok := s.store.get(key); !ok || sess.KeyFP != s.keyFP {
			continue
		}
		if err := s.recoverSession(ctx, key); err != nil {
			failed = append(failed, s.sessionFailed(key, err))
		} else {
			s.clearRefused(key)
		}
	}
	var pending []string
	for _, f := range failed {
		if f != "" {
			pending = append(pending, f)
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("%d session(s) not recovered yet: %s", len(pending), strings.Join(pending, "; "))
	}
	return nil
}

// sessionFailed records one failed recovery of key and returns the reason,
// for the retry — or retires the session and returns "" (nothing left to
// retry) when it holds an open intent whose recovery Hermes has DEFINITELY
// refused, at every attempt, for retireAfter. Anything that may pass is only
// ever retried, and restarts that clock: two refusals with a 5xx between them
// are not a refusal that persisted.
func (s *server) sessionFailed(key string, cause error) string {
	reason := fmt.Sprintf("%q: %v", key, cause)
	sess, ok := s.store.get(key)
	if !ok || sess.Intent == nil || !definiteRefusal(cause) {
		s.clearRefused(key)
		return reason
	}
	s.mu.Lock()
	since, seen := s.refusedSince[key]
	if !seen {
		since = s.now()
		s.refusedSince[key] = since
	}
	s.mu.Unlock()
	if s.now().Sub(since) < s.retireAfter {
		return reason
	}
	notice := fmt.Sprintf("The previous Hermes session could not be recovered (%v); this turn starts a new session "+
		"without its history. Your previous message was not confirmed: %s.", cause, mayHaveRunText)
	if err := s.store.retire(key, s.keyFP, notice); err != nil {
		return reason + " (retiring it also failed: " + err.Error() + ")"
	}
	s.logf("hermesd: recovery: RETIRED session %q, its recovery refused for %s: %v", key, s.now().Sub(since).Round(time.Second), cause)
	s.clearRefused(key)
	return ""
}

// clearRefused ends key's run of definite refusals (refusedSince).
func (s *server) clearRefused(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.refusedSince, key)
}

// definiteRefusal: Hermes answered the recovery call with a 4xx that retrying
// will not change (not 404, which recovery handles, nor 408 or 429), and that
// refuses THIS call rather than the API key (refusesTheKey).
func definiteRefusal(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	switch {
	case ae.Status < 400 || ae.Status >= 500:
		return false
	case ae.Status == http.StatusNotFound, ae.Status == http.StatusRequestTimeout, ae.Status == http.StatusTooManyRequests:
		return false
	case refusesTheKey(ae.Status):
		return false
	}
	return true
}

// refusesTheKey: a 401 or 403 refuses hermesd's API key, and so every call
// alike. It says nothing about the session or run the call named, nor — the
// key being checked before Hermes's idempotency store — about whether an
// earlier POST was admitted. It may pass (the key is put right): retried.
func refusesTheKey(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// recoverSession settles key's open intent (resolving its run when Hermes
// left it unknown), unless a turn is serving the session now. A session with
// no open intent has nothing to recover and makes no call: its last run became
// its last run only once it had ended.
func (s *server) recoverSession(ctx context.Context, key string) error {
	sess, ok := s.store.get(key)
	if !ok || sess.Intent == nil {
		return nil
	}
	if s.isLive(key) {
		// The turn serving it closes it; a run it follows that was left unknown
		// is resolved here, so the turn's poll sees it end.
		if sess.Intent.RunID != "" {
			cctx, cancel := context.WithTimeout(ctx, recoveryCallTimeout)
			defer cancel()
			if _, err := s.resolveIfUnknown(cctx, sess.Intent.RunID); err != nil && !isAPIError(err, http.StatusNotFound, "") {
				return fmt.Errorf("session %s: run %s: %w", sess.HermesID, sess.Intent.RunID, err)
			}
		}
		return nil
	}
	return s.recoverIntent(ctx, key, sess)
}

// settleIntent settles key's open intent for a turn that found one on its
// session, by recovery's rules. Its errors are logged: the turn then answers
// busy.
func (s *server) settleIntent(ctx context.Context, key string) {
	sess, ok := s.store.get(key)
	if !ok || sess.Intent == nil || sess.Intent.KeyFP != s.keyFP {
		return
	}
	if err := s.recoverIntent(ctx, key, sess); err != nil {
		s.logf("hermesd: settle the open intent on %q: %v", key, err)
	}
}

// recoverIntent settles sess's open intent (posted under the current key).
func (s *server) recoverIntent(ctx context.Context, key string, sess session) error {
	in := sess.Intent
	if notice, ok := s.tombstone(in.IdempotencyKey); ok {
		// Hermes refused it; only the close failed to reach the disk. Never
		// re-posted: close it again.
		return s.closeRefused(key, in.IdempotencyKey, notice)
	}
	cctx, cancel := context.WithTimeout(ctx, recoveryCallTimeout)
	defer cancel()
	if in.RunID != "" {
		st, err := s.resolveIfUnknown(cctx, in.RunID)
		switch {
		case isAPIError(err, http.StatusNotFound, ""):
			return s.store.closeIntent(key, in.IdempotencyKey, "Your previous message's run ("+in.RunID+") is no longer known to Hermes; "+mayHaveRunText+".")
		case err != nil:
			return fmt.Errorf("session %s: run %s: %w", sess.HermesID, in.RunID, err)
		case terminalStatus(st.Status):
			return s.store.closeIntent(key, in.IdempotencyKey, "")
		}
		return nil // still running: the intent stays open
	}
	if s.now().Sub(in.At) > intentRetention {
		notice, err := s.resolveFromMessages(cctx, sess)
		if err != nil {
			return err
		}
		s.logf("hermesd: recovery: an intent on session %s is older than %s; not re-posted", sess.HermesID, intentRetention)
		return s.store.closeIntent(key, in.IdempotencyKey, notice)
	}
	return s.replay(cctx, key, *in)
}

// replay re-posts an intent exactly as it was first posted, and records the
// run it was admitted as (whose intent then stays open until it ends).
func (s *server) replay(ctx context.Context, key string, in intent) error {
	runID, replayed, err := s.api.postRun(ctx, in.IdempotencyKey, in.Body)
	switch {
	case isAPIError(err, http.StatusConflict, "idempotency_key_conflict"):
		return s.store.closeIntent(key, in.IdempotencyKey,
			"Your previous message "+mayHaveRunText+" (Hermes says its idempotency key was used with a different request).")
	case err != nil:
		var ae *apiError
		if errors.As(err, &ae) && ae.Status < 500 && ae.Status != http.StatusTooManyRequests && !refusesTheKey(ae.Status) {
			// Refused outright: it never ran. (Not a 401/403: the key was
			// refused before the idempotency store was read, so an earlier POST
			// of this intent may have been admitted.)
			return s.closeRefused(key, in.IdempotencyKey, "Your previous message was not run: Hermes refused it ("+ae.Error()+").")
		}
		return fmt.Errorf("replay the intent on %s: %w", key, err)
	}
	s.logf("hermesd: recovery: re-posted an unrecorded intent as run %s (replayed=%t)", runID, replayed)
	return s.store.setRunID(key, in.IdempotencyKey, runID)
}

// resolveFromMessages decides what to say about an intent that cannot be
// re-posted, from its session's messages: a user message carrying the intent's
// input means it reached Hermes.
func (s *server) resolveFromMessages(ctx context.Context, sess session) (string, error) {
	var req runRequest
	_ = json.Unmarshal(sess.Intent.Body, &req)
	cctx, cancel := context.WithTimeout(ctx, recoveryCallTimeout)
	defer cancel()
	msgs, err := s.api.sessionMessages(cctx, sess.HermesID)
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

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// tombstoneRetry is how often a refusal whose close could not be written is
// written again (a variable so a test can shorten it).
var tombstoneRetry = 2 * time.Second

// closeRefused closes an intent Hermes DEFINITELY refused (it never ran).
//
// 🔴 A DEFINITE REFUSAL NEVER STAYS A REPLAYABLE INTENT. When the close cannot
// be written, the intent is still open on disk (the store writes before it
// changes memory), and settleIntent or recovery would re-post it — and Hermes,
// which never admitted that key, would run a turn it had refused. So a
// TOMBSTONE is kept for its idempotency key: recoverIntent closes a tombstoned
// intent instead of re-posting it, and a retry loop writes the close until it
// lands. The tombstone is memory only, so it cannot cover a crash before the
// close lands; the caller must not promise "not sent" when this returns an
// error (runTurn answers may_have_run).
func (s *server) closeRefused(key, idem, notice string) error {
	err := s.store.closeIntent(key, idem, notice)
	if err == nil {
		s.clearTombstone(idem)
		return nil
	}
	s.mu.Lock()
	_, retrying := s.tombstones[idem]
	s.tombstones[idem] = notice
	s.mu.Unlock()
	s.logf("hermesd: Hermes refused the run of %s but the close could not be written (%v); it is tombstoned, never re-posted, and retried", key, err)
	if !retrying {
		go s.retryClose(key, idem, notice)
	}
	return err
}

func (s *server) retryClose(key, idem, notice string) {
	for {
		time.Sleep(tombstoneRetry)
		s.mu.Lock()
		closing := s.closing
		_, still := s.tombstones[idem]
		s.mu.Unlock()
		if closing || !still {
			return
		}
		if err := s.store.closeIntent(key, idem, notice); err == nil {
			s.clearTombstone(idem)
			s.logf("hermesd: the refused run of %s is closed", key)
			return
		}
	}
}

func (s *server) tombstone(idem string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.tombstones[idem]
	return n, ok
}

func (s *server) clearTombstone(idem string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tombstones, idem)
}
