package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"
)

// The session map: hermesd's memory of every muster session, on the volume.
//
// A muster session key (the X-Openclaw-Session-Key muster sends on every turn;
// "" for a turn that names none) maps to ONE Hermes session id, the fingerprint
// of the API key that session's runs were posted under, its last run, and — at
// most one — an INTENT: a turn hermesd decided to post and has not seen settle.
//
// 🔴 THE INTENT IS WRITTEN, AND SYNCED, BEFORE `POST /v1/runs`. It holds the
// idempotency key the POST carries, the EXACT request body bytes and the key's
// fingerprint. A crash between the POST and the record of its run id would
// otherwise leave a run hermesd cannot name, parking the session behind it for
// good; with the intent, recovery re-posts the same key and the same bytes, and
// Hermes's idempotency store replays the run it already admitted rather than
// starting a second (its request fingerprint hashes the body,
// P:gateway/platforms/api_server_runs.py:660-666, so only the identical body
// replays). recover.go is the reader.
//
// The file is rewritten whole, atomically, on every change: the map holds one
// small entry per session muster ever opened on this agent.

// intent is one turn hermesd posted (or is about to post) and has not settled.
type intent struct {
	IdempotencyKey string    `json:"idempotency_key"`
	Body           []byte    `json:"body"` // the exact bytes POSTed (and re-POSTed)
	KeyFP          string    `json:"key_fingerprint"`
	At             time.Time `json:"at"`
	RunID          string    `json:"run_id,omitempty"` // from the 202; "" while posting
}

// session is one muster session's Hermes side.
type session struct {
	HermesID  string  `json:"hermes_session_id"`
	KeyFP     string  `json:"key_fingerprint"`
	LastRunID string  `json:"last_run_id,omitempty"`
	Intent    *intent `json:"intent,omitempty"`
	// Notice is said at the start of the next reply on this session: what
	// recovery decided about a turn nobody was waiting for any more, or that the
	// session was retired.
	Notice string `json:"notice,omitempty"`
}

type storeFile struct {
	Version  int                 `json:"version"`
	Sessions map[string]*session `json:"sessions"`
}

// store is the session map.
//
// 🔴 EVERY CHANGE IS WRITTEN TO DISK BEFORE IT REACHES MEMORY. A change is made
// to a COPY of the map, the copy is written, and only a write that succeeded
// replaces the map in memory (commit). A failed write leaves memory exactly as
// the file is. Mutating memory first and writing second left, on a failed
// write, an intent in memory that the turn had reported as never sent — and the
// NEXT successful write (any session's) persisted it, so recovery then POSTed a
// turn muster had been told was not sent.
type store struct {
	path string

	mu       sync.Mutex
	sessions map[string]*session
}

func openStore(path string) (*store, error) {
	s := &store{path: path, sessions: map[string]*session{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("hermesd: read the session map: %w", err)
	}
	var f storeFile
	if err := json.Unmarshal(b, &f); err != nil {
		// 🔴 A MAP hermesd CANNOT READ IS NOT REPLACED BY AN EMPTY ONE: that
		// would forget every intent and every session's run, which is exactly
		// what recovery exists to remember. The pod fails to start, naming it.
		return nil, fmt.Errorf("hermesd: the session map %s is not readable JSON (%v); move it aside to start over", path, err)
	}
	if f.Sessions != nil {
		s.sessions = f.Sessions
	}
	return s, nil
}

// commit applies mutate to a copy of the map; when mutate reports a change,
// the copy is written and, only once the write succeeded, becomes the map. The
// caller does not hold mu.
func (s *store) commit(mutate func(next map[string]*session) (changed bool, err error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]*session, len(s.sessions))
	for k, v := range s.sessions {
		c := copySession(v)
		next[k] = &c
	}
	changed, err := mutate(next)
	if err != nil || !changed {
		return err
	}
	b, err := json.MarshalIndent(storeFile{Version: 1, Sessions: next}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.path, b, 0o600); err != nil {
		return err
	}
	s.sessions = next
	return nil
}

// get returns a copy of the session for key, or ok=false.
func (s *store) get(key string) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[key]
	if !ok {
		return session{}, false
	}
	return copySession(ss), true
}

func copySession(ss *session) session {
	c := *ss
	if ss.Intent != nil {
		in := *ss.Intent
		in.Body = append([]byte(nil), ss.Intent.Body...)
		c.Intent = &in
	}
	return c
}

// keys lists every session key in the map.
func (s *store) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.sessions))
	for k := range s.sessions {
		out = append(out, k)
	}
	return out
}

// ensure returns the session for key, creating it — a fresh Hermes session id
// under keyFP — when the map has none.
func (s *store) ensure(key, keyFP string) (session, error) {
	if ss, ok := s.get(key); ok {
		return ss, nil
	}
	err := s.commit(func(next map[string]*session) (bool, error) {
		if _, ok := next[key]; ok {
			return false, nil
		}
		next[key] = &session{HermesID: newHermesSessionID(), KeyFP: keyFP}
		return true, nil
	})
	if err != nil {
		return session{}, err
	}
	ss, _ := s.get(key)
	return ss, nil
}

// errSessionBusy is beginIntent's refusal: the session's last turn has not
// settled (it is still running in Hermes). It is the ONE place the open-intent
// rule is checked; the turn answers 409 busy.
var errSessionBusy = errors.New("a turn on this session has not settled yet (it is still running in Hermes)")

// beginIntent records in as key's open intent, durably — or not at all. It
// refuses when the session already has one: that turn has not settled.
func (s *store) beginIntent(key string, in intent) error {
	return s.commit(func(next map[string]*session) (bool, error) {
		ss, ok := next[key]
		if !ok {
			return false, fmt.Errorf("hermesd: no session %q", key)
		}
		if ss.Intent != nil {
			return false, errSessionBusy
		}
		ss.Intent = &in
		return true, nil
	})
}

// setRunID records the run the open intent (idempotency key idem) was admitted
// as.
func (s *store) setRunID(key, idem, runID string) error {
	return s.commit(func(next map[string]*session) (bool, error) {
		ss, ok := next[key]
		if !ok || ss.Intent == nil || ss.Intent.IdempotencyKey != idem {
			return false, nil
		}
		ss.Intent.RunID = runID
		return true, nil
	})
}

// closeIntent ends the open intent (idempotency key idem): its run, if it has
// one, becomes the session's last run. A notice, when non-empty, is kept for
// the session's next reply. Closing an intent that is no longer open is a no-op.
func (s *store) closeIntent(key, idem, notice string) error {
	return s.commit(func(next map[string]*session) (bool, error) {
		ss, ok := next[key]
		if !ok || ss.Intent == nil || ss.Intent.IdempotencyKey != idem {
			return false, nil
		}
		if ss.Intent.RunID != "" {
			ss.LastRunID = ss.Intent.RunID
		}
		ss.Intent = nil
		ss.Notice = joinNotice(ss.Notice, notice)
		return true, nil
	})
}

// takeNotice returns and clears key's notice.
func (s *store) takeNotice(key string) (string, error) {
	var n string
	err := s.commit(func(next map[string]*session) (bool, error) {
		ss, ok := next[key]
		if !ok || ss.Notice == "" {
			return false, nil
		}
		n, ss.Notice = ss.Notice, ""
		return true, nil
	})
	if err != nil {
		return "", err
	}
	return n, nil
}

// retire maps key to a fresh Hermes session under keyFP, forgetting its last
// run (and any intent), and leaves notice for its next reply.
func (s *store) retire(key, keyFP, notice string) error {
	return s.commit(func(next map[string]*session) (bool, error) {
		ss, ok := next[key]
		if !ok {
			return false, nil
		}
		ss.HermesID, ss.KeyFP, ss.LastRunID, ss.Intent = newHermesSessionID(), keyFP, "", nil
		ss.Notice = joinNotice(ss.Notice, notice)
		return true, nil
	})
}

func joinNotice(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "\n\n" + b
}

// newHermesSessionID mints a Hermes session id. hermesd always names the
// session a run goes to: Hermes would otherwise default it to the run's own id
// (S6), a new session per turn.
func newHermesSessionID() string { return "muster-" + randomHex(12) }

func newIdempotencyKey() string { return "hermesd-" + randomHex(16) }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("hermesd: crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}
