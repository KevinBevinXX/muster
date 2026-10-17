package askq

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"maps"
	"regexp"
	"sort"
	"sync"
	"time"
)

// THE PENDING-QUESTION STORE: the questions a hook is holding a Claude Code
// session on, waiting for a person to answer them in muster.
//
// 🔴 IN MEMORY, AND THAT IS THE FAIL-OPEN PATH, NOT A GAP. A question is only
// ever pending while ONE hook process holds its call (bounded by the hook's
// own deadline), and the hook polls. A muster restart forgets every pending
// question; the hook's next poll gets 404 and it leaves the call to the
// terminal's own picker — exactly what it does when muster cannot be reached.
// Nothing here is a record anyone needs later: the transcript is the history
// (the call's toolUseResult holds the questions and the answers).
//
// 🔴 THE HOOK IS THE ONLY WRITER OF A QUESTION, A PERSON THE ONLY WRITER OF AN
// ANSWER. Open is the hook's (authenticated by its source as an owner and a
// scope); Answer takes the owner of the request that answers it, and refuses an
// entry of any other owner as not found. Nothing answers a question on its own.
//
// 🔴 AN ANSWER IS TAKEN ONLY WHILE THE HOOK IS STILL WAITING FOR IT. muster
// must never say "sent to Claude" for answers no hook will collect. So a
// question is ALIVE only while its hook has a poll in flight or polled within
// PollGrace; past that it is withdrawn (Reason ReasonStopped: the hook was
// killed, or lost muster) and an answer is refused. The other half is the
// hook's: before it gives up it withdraws the question and reads the state the
// withdraw returns — answers that arrived in that window are still delivered
// (hook.go).

// State is where a question is.
type State string

const (
	// Pending: a hook is holding the call; nobody has answered.
	Pending State = "pending"
	// Answered: a person answered it in muster; the hook hands the answers to
	// the CLI.
	Answered State = "answered"
	// Withdrawn: the hook gave up holding the call (its deadline, a signal,
	// or it stopped polling), so the terminal's own picker is showing it — it is
	// answered there, if at all.
	Withdrawn State = "withdrawn"
)

// Why a question was withdrawn (Entry.Reason).
const (
	// ReasonGaveUp: the hook withdrew it (its hold ended, a signal, or it
	// could not reach muster).
	ReasonGaveUp = "gave-up"
	// ReasonTimeout: its hold's deadline passed.
	ReasonTimeout = "timeout"
	// ReasonStopped: the hook stopped polling — it was killed, or cannot reach
	// muster — so the session stopped waiting here.
	ReasonStopped = "stopped"
)

// Key names one question set: whose it is, which surface asked (Source:
// "mirror"), within what (Scope: the mirror host's name), and the Claude Code
// call (Session: the Claude session id; ToolUseID: the call's tool_use id).
type Key struct {
	Owner     int64
	Source    string
	Scope     string
	Session   string
	ToolUseID string
}

// Entry is one question set and where it is.
type Entry struct {
	ID        string
	Key       Key
	Questions []Question
	State     State
	// Reason is why a Withdrawn entry was withdrawn ("" otherwise).
	Reason string
	// Answers is set once State is Answered.
	Answers  map[string]string
	Created  time.Time
	Deadline time.Time
	Resolved time.Time
}

// The store's bounds and clocks.
const (
	// MaxHold bounds how long a hook may hold one call.
	MaxHold = 30 * time.Minute
	// HookWait is the long-poll the hook asks for, and PollGrace how long a
	// hook may go with no poll in flight before the question is taken to be
	// abandoned: 1.5 × HookWait.
	HookWait  = 20 * time.Second
	PollGrace = HookWait * 3 / 2
	// Retain is how long a resolved question is kept, so a page that loads
	// after the answer still shows how it went.
	Retain = 15 * time.Minute
	// MaxPendingPerOwner bounds one owner's pending questions,
	// MaxEntriesPerOwner all of one owner's entries (pending and resolved),
	// and MaxEntries the store's whole size.
	MaxPendingPerOwner = 32
	MaxEntriesPerOwner = 256
	MaxEntries         = 4096
)

// idPattern is an entry id: 128 random bits, hex — the hook's own choice
// (Open), or the store's.
var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidID reports whether id is an entry id.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// NewID is a fresh entry id.
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("askq: no randomness: " + err.Error())
	}
	return hex.EncodeToString(b)
}

var (
	// ErrNotFound: no such question this caller may see.
	ErrNotFound = errors.New("askq: no such question")
	// ErrNotPending: the question was withdrawn (it went back to the terminal).
	ErrNotPending = errors.New("askq: this question went back to the terminal, so it can only be answered there")
	// ErrAnswered: the question was already answered, differently.
	ErrAnswered = errors.New("askq: this question was already answered")
	// ErrFull: too many questions are pending.
	ErrFull = errors.New("askq: too many questions are waiting")
	// ErrIDTaken: the id a hook chose names another question.
	ErrIDTaken = errors.New("askq: that question id is taken")
)

type entry struct {
	Entry
	lastSeen time.Time
	polling  int           // polls in flight
	changed  chan struct{} // closed (and replaced) on every change
}

// alive: the entry's hook has a poll in flight or polled within PollGrace.
func (e *entry) alive(now time.Time) bool {
	return e.polling > 0 || now.Sub(e.lastSeen) <= PollGrace
}

// Store is the pending-question store. The zero value is not usable; New.
type Store struct {
	mu    sync.Mutex
	byID  map[string]*entry
	byKey map[Key]string
	now   func() time.Time
	// onChange, when set, is called OUTSIDE the lock with every entry that
	// was opened or changed state.
	onChange func(Entry)
}

// New returns an empty store.
func New() *Store {
	return &Store{byID: map[string]*entry{}, byKey: map[Key]string{}, now: time.Now}
}

// SetNow replaces the clock (tests).
func (s *Store) SetNow(now func() time.Time) {
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
}

// OnChange installs the change callback: a question was opened, answered or
// withdrawn.
func (s *Store) OnChange(fn func(Entry)) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

func (e *entry) snapshot() Entry {
	out := e.Entry
	out.Questions = append([]Question(nil), e.Questions...)
	out.Answers = maps.Clone(e.Answers)
	return out
}

// touch records a change: wakes waiters and queues the callback.
func (s *Store) touch(e *entry, fire *[]Entry) {
	close(e.changed)
	e.changed = make(chan struct{})
	*fire = append(*fire, e.snapshot())
}

func (s *Store) notify(fire []Entry) {
	s.mu.Lock()
	fn := s.onChange
	s.mu.Unlock()
	if fn == nil {
		return
	}
	for _, e := range fire {
		fn(e)
	}
}

func (s *Store) withdrawLocked(e *entry, reason string, fire *[]Entry) {
	e.State, e.Reason, e.Resolved = Withdrawn, reason, s.now()
	s.touch(e, fire)
}

// sweepLocked withdraws pending entries past their deadline or whose hook has
// stopped polling, and forgets resolved ones past Retain.
func (s *Store) sweepLocked(fire *[]Entry) {
	now := s.now()
	for id, e := range s.byID {
		switch {
		case e.State == Pending && now.After(e.Deadline):
			s.withdrawLocked(e, ReasonTimeout, fire)
		case e.State == Pending && !e.alive(now):
			s.withdrawLocked(e, ReasonStopped, fire)
		case e.State != Pending && now.Sub(e.Resolved) > Retain:
			s.removeLocked(id, e)
		}
	}
}

func (s *Store) removeLocked(id string, e *entry) {
	delete(s.byID, id)
	if s.byKey[e.Key] == id {
		delete(s.byKey, e.Key)
	}
}

// evictLocked makes room for one more entry of owner: past
// MaxEntriesPerOwner it drops THAT owner's oldest resolved entries, and past
// MaxEntries the store's oldest resolved ones. A pending entry is never
// evicted; with no resolved entry to drop, it refuses (ErrFull). One owner can
// therefore never fill the store against another: its own resolved entries go
// first, and its pending ones are bounded by MaxPendingPerOwner.
func (s *Store) evictLocked(owner int64) error {
	oldest := func(match func(*entry) bool) (string, *entry) {
		var id string
		var o *entry
		for i, e := range s.byID {
			if e.State != Pending && match(e) && (o == nil || e.Resolved.Before(o.Resolved)) {
				id, o = i, e
			}
		}
		return id, o
	}
	count := func(match func(*entry) bool) int {
		n := 0
		for _, e := range s.byID {
			if match(e) {
				n++
			}
		}
		return n
	}
	mine := func(e *entry) bool { return e.Key.Owner == owner }
	for count(mine) >= MaxEntriesPerOwner {
		id, e := oldest(mine)
		if e == nil {
			return ErrFull
		}
		s.removeLocked(id, e)
	}
	for len(s.byID) >= MaxEntries {
		id, e := oldest(func(*entry) bool { return true })
		if e == nil {
			return ErrFull
		}
		s.removeLocked(id, e)
	}
	if count(func(e *entry) bool { return mine(e) && e.State == Pending }) >= MaxPendingPerOwner {
		return ErrFull
	}
	return nil
}

// Open registers qs for key under id — the hook's own choice, so a retried or
// timed-out registration names the same entry and can be withdrawn; "" lets the
// store choose — held for at most hold (capped at MaxHold).
//
// Opening an id that already names key's entry returns it, whatever its state
// (the hook's retry reads it); an id naming another key is ErrIDTaken. Opening
// a key that is already PENDING under another id returns that entry; opening
// one that was resolved starts a new entry (the same call asked again).
func (s *Store) Open(key Key, qs []Question, hold time.Duration, id string) (Entry, error) {
	if key.Owner <= 0 || key.Source == "" || key.Session == "" || key.ToolUseID == "" {
		return Entry{}, invalid("a question needs an owner, a source, a session and a tool_use id")
	}
	if len(qs) == 0 {
		return Entry{}, invalid("no questions")
	}
	if id != "" && !ValidID(id) {
		return Entry{}, invalid("a question id is 32 lowercase hex digits")
	}
	if hold <= 0 || hold > MaxHold {
		hold = MaxHold
	}
	var fire []Entry
	defer func() { s.notify(fire) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(&fire)
	now := s.now()
	if e, ok := s.byID[id]; ok && id != "" {
		if e.Key != key {
			return Entry{}, ErrIDTaken
		}
		if e.State == Pending {
			e.lastSeen = now
		}
		return e.snapshot(), nil
	}
	if cur, ok := s.byKey[key]; ok {
		if e := s.byID[cur]; e != nil && e.State == Pending {
			e.lastSeen = now
			return e.snapshot(), nil
		}
	}
	if err := s.evictLocked(key.Owner); err != nil {
		return Entry{}, err
	}
	if id == "" {
		id = NewID()
	}
	e := &entry{Entry: Entry{ID: id, Key: key, Questions: append([]Question(nil), qs...), State: Pending,
		Created: now, Deadline: now.Add(hold)}, lastSeen: now, changed: make(chan struct{})}
	s.byID[e.ID] = e
	s.byKey[key] = e.ID
	fire = append(fire, e.snapshot())
	return e.snapshot(), nil
}

// Get is entry id, if the store has it.
func (s *Store) Get(id string) (Entry, bool) {
	var fire []Entry
	defer func() { s.notify(fire) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(&fire)
	e, ok := s.byID[id]
	if !ok {
		return Entry{}, false
	}
	return e.snapshot(), true
}

// holds reports whether e is the question of the holder (owner, source,
// scope).
func (e *entry) holds(owner int64, source, scope string) bool {
	return e.Key.Owner == owner && e.Key.Source == source && e.Key.Scope == scope
}

// Poll is the hook's read of entry id, as the holder of key's owner and scope:
// it marks the hook alive (for as long as the poll is in flight), then waits up
// to wait for the entry to leave Pending. A key that does not match is
// ErrNotFound, like an unknown id.
func (s *Store) Poll(ctx context.Context, id string, owner int64, source, scope string, wait time.Duration) (Entry, error) {
	var fire []Entry
	s.mu.Lock()
	s.sweepLocked(&fire)
	e, ok := s.byID[id]
	if !ok || !e.holds(owner, source, scope) {
		s.mu.Unlock()
		s.notify(fire)
		return Entry{}, ErrNotFound
	}
	e.lastSeen = s.now()
	snap, ch := e.snapshot(), e.changed
	if snap.State != Pending || wait <= 0 {
		s.mu.Unlock()
		s.notify(fire)
		return snap, nil
	}
	e.polling++
	s.mu.Unlock()
	s.notify(fire)
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	case <-ctx.Done():
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e.polling--
	// A poll the hook abandoned (its connection closed) is no sign of life.
	if ctx.Err() == nil {
		e.lastSeen = s.now()
	}
	if cur, ok := s.byID[id]; ok {
		return cur.snapshot(), nil
	}
	return Entry{}, ErrNotFound
}

// Withdraw is the hook giving up on entry id (as the holder of owner, source
// and scope): a pending entry becomes Withdrawn. It returns the entry as it is
// after — an entry a person ANSWERED before the withdraw arrived stays
// Answered, and the hook hands those answers to the CLI after all.
func (s *Store) Withdraw(id string, owner int64, source, scope string) (Entry, error) {
	var fire []Entry
	defer func() { s.notify(fire) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok || !e.holds(owner, source, scope) {
		return Entry{}, ErrNotFound
	}
	if e.State == Pending {
		s.withdrawLocked(e, ReasonGaveUp, &fire)
	}
	return e.snapshot(), nil
}

// Find is key's newest entry, if the store has one.
func (s *Store) Find(key Key) (Entry, bool) {
	var fire []Entry
	defer func() { s.notify(fire) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(&fire)
	e, ok := s.byID[s.byKey[key]]
	if !ok {
		return Entry{}, false
	}
	return e.snapshot(), true
}

// Answer records answers to the entry at key, as owner (key.Owner is the
// request's own owner; another owner's entry is ErrNotFound). The answers must
// already have been checked against the entry's questions (FromForm); they are
// checked again here.
//
// 🔴 REPLAY-SAFE: answering an Answered entry again with the SAME answers is
// success (a double submit, a retry whose first response was lost); with
// different ones it is ErrAnswered. A Withdrawn entry — including one whose
// hook stopped polling (the sweep below withdraws it first) — is
// ErrNotPending: the terminal has it now.
func (s *Store) Answer(key Key, answers map[string]string) (Entry, error) {
	var fire []Entry
	defer func() { s.notify(fire) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(&fire)
	e, ok := s.byID[s.byKey[key]]
	if !ok {
		return Entry{}, ErrNotFound
	}
	if err := CheckAnswers(e.Questions, answers); err != nil {
		return Entry{}, err
	}
	switch e.State {
	case Answered:
		if maps.Equal(e.Answers, answers) {
			return e.snapshot(), nil
		}
		return e.snapshot(), ErrAnswered
	case Withdrawn:
		return e.snapshot(), ErrNotPending
	}
	e.State, e.Answers, e.Resolved = Answered, maps.Clone(answers), s.now()
	s.touch(e, &fire)
	return e.snapshot(), nil
}

// Pending is owner's pending questions, oldest first: what an attention view
// lists as "waiting on you".
func (s *Store) Pending(owner int64) []Entry {
	var fire []Entry
	defer func() { s.notify(fire) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(&fire)
	var out []Entry
	for _, e := range s.byID {
		if e.Key.Owner == owner && e.State == Pending {
			out = append(out, e.snapshot())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// ForSession is owner's questions in (source, scope, session) that are
// pending, or were resolved within the last shownFor — oldest first: what a
// view of that session shows.
func (s *Store) ForSession(owner int64, source, scope, session string, shownFor time.Duration) []Entry {
	var fire []Entry
	defer func() { s.notify(fire) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(&fire)
	now := s.now()
	var out []Entry
	for _, e := range s.byID {
		if e.Key.Owner != owner || e.Key.Source != source || e.Key.Scope != scope || e.Key.Session != session {
			continue
		}
		if e.State == Pending || now.Sub(e.Resolved) <= shownFor {
			out = append(out, e.snapshot())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// Waiting reports whether owner has a pending question in (source, scope,
// session) whose hook is still waiting: the session is waiting on a person.
func (s *Store) Waiting(owner int64, source, scope, session string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, e := range s.byID {
		if e.State == Pending && e.Key.Owner == owner && e.Key.Source == source && e.Key.Scope == scope && e.Key.Session == session &&
			!now.After(e.Deadline) && e.alive(now) {
			return true
		}
	}
	return false
}

// Sweep runs the expiry rules now, firing OnChange for every question it
// withdraws — so a view that shows "waiting" learns promptly that a hook died
// between reads (RunSweep runs it on a ticker).
func (s *Store) Sweep() {
	var fire []Entry
	s.mu.Lock()
	s.sweepLocked(&fire)
	s.mu.Unlock()
	s.notify(fire)
}

// SweepEvery is RunSweep's period.
const SweepEvery = 5 * time.Second

// RunSweep runs Sweep every interval until ctx ends.
func (s *Store) RunSweep(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = SweepEvery
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sweep()
		}
	}
}
