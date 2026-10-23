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

// THE QUESTION STORE: the AskUserQuestion calls sessions are asking — each on
// screen in its terminal's picker AND in muster at once — and how each ended.
//
// 🔴 IN MEMORY, AND NOTHING HERE IS NEEDED TO ANSWER. The terminal's picker is
// always there; the store is what lets muster show the question and type an
// answer into that picker. A muster restart forgets the open questions: their
// pickers stay on screen, unaffected. The transcript is the history.
//
// 🔴 HOW A QUESTION ENDS IS THE CLI'S WORD, NOT MUSTER'S. Resolve records what
// the transcript says the call returned (its toolUseResult); an answer typed
// from muster is only "typing" until then. Whichever answer the CLI took is the
// answer shown — muster's, or the one given in the terminal first.

// State is where a question is.
type State string

const (
	// Pending: the picker is on screen; nobody has answered.
	Pending State = "pending"
	// Typing: muster is typing a person's answer into the picker; the CLI has
	// not recorded a result yet.
	Typing State = "typing"
	// Failed: muster could not type the answer in (Note says why); the picker
	// is still the terminal's to answer.
	Failed State = "failed"
	// Answered: the CLI recorded answers (By says whose they were).
	Answered State = "answered"
	// Declined: the CLI recorded the call ended with no answers (the picker
	// was dismissed in the terminal).
	Declined State = "declined"
	// Expired: no result was seen within MaxOpen, or the session went away
	// (Gone; Note says so).
	Expired State = "expired"
	// Superseded: a newer question was asked in the same Claude session, so
	// this one's picker is no longer on screen; it is never typed into.
	Superseded State = "superseded"
)

// Open reports whether a question in state s is still unanswered.
func (s State) Open() bool { return s == Pending || s == Typing || s == Failed }

// Who answered (Entry.By).
const (
	ByMuster   = "muster"
	ByTerminal = "terminal"
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
	// Answers is the recorded answers (Answered); Typed the answers muster is
	// typing or typed (Typing, and kept after); By who answered (Answered).
	Answers map[string]string
	Typed   map[string]string
	By      string
	// Note is why typing failed (Failed).
	Note string
	// Retry (Failed) says the failure leaves the question answerable from
	// muster again: typing can be tried again (Failure.Retry).
	Retry bool
	// Left is what muster's typing is KNOWN to have left in the terminal's
	// picker: the choices it was typing and how many keys of their plan it
	// sent (nil: nothing known to be there). A drive that stopped part way
	// leaves it; the next drive is handed it (BeginTyping) so it can tell
	// muster's own partial answer from someone else's and continue it.
	Left     *Progress
	Created  time.Time
	Resolved time.Time
}

// Progress is how far muster's typing of an answer got into the terminal's
// picker: the choices it was typing, and how many keys of their plan
// (internal/askdrive's Plan) it sent.
type Progress struct {
	Choices []Choice `json:"choices"`
	Keys    int      `json:"keys"`
}

// clone is a deep copy of p (nil for nil).
func (p *Progress) clone() *Progress {
	if p == nil {
		return nil
	}
	out := &Progress{Keys: p.Keys, Choices: make([]Choice, len(p.Choices))}
	for i, c := range p.Choices {
		out.Choices[i] = Choice{Picks: append([]int(nil), c.Picks...), Other: c.Other}
	}
	return out
}

// The store's bounds and clocks.
const (
	// MaxOpen is how long a question stays open with no result seen.
	MaxOpen = 6 * time.Hour
	// Retain is how long a resolved question is kept, so a page that loads
	// after it still shows how it went.
	Retain = 15 * time.Minute
	// MaxPendingPerOwner bounds one owner's open questions,
	// MaxEntriesPerOwner all of one owner's entries (open and resolved), and
	// MaxEntries the store's whole size.
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
	// ErrClosed: the question was answered (or dismissed) already.
	ErrClosed = errors.New("askq: this question was already answered")
	// ErrBusy: an answer is being typed in for it already.
	ErrBusy = errors.New("askq: an answer is already being typed in for this question")
	// ErrFull: too many questions are open.
	ErrFull = errors.New("askq: too many questions are waiting")
	// ErrIDTaken: the id a hook chose names another question.
	ErrIDTaken = errors.New("askq: that question id is taken")
)

type entry struct {
	Entry
	// driveUntil: a host may still be typing this question's answer until
	// then — its last typing ended without the host's outcome
	// (TypingFailed, settled false). Zero once the outcome is known.
	driveUntil time.Time
}

// MaxDrive bounds how long a host can be typing one answer: longer than the
// hub waits for the host's outcome (60 s) and the host's own bound (50 s).
const MaxDrive = 90 * time.Second

// Store is the question store. The zero value is not usable; New.
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

// OnChange installs the change callback: a question was opened or changed.
func (s *Store) OnChange(fn func(Entry)) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

func (e *entry) snapshot() Entry {
	out := e.Entry
	out.Questions = append([]Question(nil), e.Questions...)
	out.Answers = maps.Clone(e.Answers)
	out.Typed = maps.Clone(e.Typed)
	out.Left = e.Left.clone()
	return out
}

// touch records a change: queues the callback.
func (s *Store) touch(e *entry, fire *[]Entry) {
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

// sweepLocked expires open questions past MaxOpen and forgets resolved ones
// past Retain.
func (s *Store) sweepLocked(fire *[]Entry) {
	now := s.now()
	for id, e := range s.byID {
		switch {
		case e.State.Open() && now.Sub(e.Created) > MaxOpen:
			e.State, e.Resolved = Expired, now
			s.touch(e, fire)
		case !e.State.Open() && now.Sub(e.Resolved) > Retain:
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
// MaxEntries the store's oldest resolved ones. An open entry is never
// evicted; with no resolved entry to drop, it refuses (ErrFull). One owner can
// therefore never fill the store against another: its own resolved entries go
// first, and its open ones are bounded by MaxPendingPerOwner.
func (s *Store) evictLocked(owner int64) error {
	oldest := func(match func(*entry) bool) (string, *entry) {
		var id string
		var o *entry
		for i, e := range s.byID {
			if !e.State.Open() && match(e) && (o == nil || e.Resolved.Before(o.Resolved)) {
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
	if count(func(e *entry) bool { return mine(e) && e.State.Open() }) >= MaxPendingPerOwner {
		return ErrFull
	}
	return nil
}

// Open registers qs for key under id — the hook's own choice, so a retried
// registration names the same entry; "" lets the store choose.
//
// Opening an id that already names key's entry returns it, whatever its state;
// an id naming another key is ErrIDTaken. Opening a key that is already OPEN
// under another id returns that entry; opening one that was resolved starts a
// new entry (the same call asked again).
func (s *Store) Open(key Key, qs []Question, id string) (Entry, error) {
	if key.Owner <= 0 || key.Source == "" || key.Session == "" || key.ToolUseID == "" {
		return Entry{}, invalid("a question needs an owner, a source, a session and a tool_use id")
	}
	if len(qs) == 0 {
		return Entry{}, invalid("no questions")
	}
	if id != "" && !ValidID(id) {
		return Entry{}, invalid("a question id is 32 lowercase hex digits")
	}
	var fire []Entry
	defer func() { s.notify(fire) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(&fire)
	if e, ok := s.byID[id]; ok && id != "" {
		if e.Key != key {
			return Entry{}, ErrIDTaken
		}
		return e.snapshot(), nil
	}
	if cur, ok := s.byKey[key]; ok {
		if e := s.byID[cur]; e != nil && e.State.Open() {
			return e.snapshot(), nil
		}
	}
	if err := s.evictLocked(key.Owner); err != nil {
		return Entry{}, err
	}
	if id == "" {
		id = NewID()
	}
	// 🔴 A NEWER QUESTION IN THE SESSION REPLACES THE OLDER ONES: Claude Code
	// shows one picker at a time, so theirs is gone — and the driver matches a
	// picker by its text, so an older card must never type into a newer
	// picker that asks the same thing.
	for _, o := range s.byID {
		if o.Key.Owner == key.Owner && o.Key.Source == key.Source && o.Key.Scope == key.Scope && o.Key.Session == key.Session &&
			o.Key.ToolUseID != key.ToolUseID && o.State.Open() {
			o.State, o.Resolved = Superseded, s.now()
			s.touch(o, &fire)
		}
	}
	e := &entry{Entry: Entry{ID: id, Key: key, Questions: append([]Question(nil), qs...), State: Pending,
		Created: s.now()}}
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

// BeginTyping marks the question at key (key.Owner is the request's own owner;
// another owner's entry is ErrNotFound) as being answered from muster with
// answers, which must fit its questions. Only a Pending question — or one whose
// last typing Failed — may begin: a question already being typed is ErrBusy,
// one the CLI has resolved ErrClosed.
//
// 🔴 REPLAY-SAFE: beginning again with the SAME answers while they are being
// typed, or once the CLI took them, returns the entry and no error and `again`
// true — the caller must then NOT type them a second time.
func (s *Store) BeginTyping(key Key, answers map[string]string) (e Entry, again bool, err error) {
	var fire []Entry
	defer func() { s.notify(fire) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(&fire)
	en, ok := s.byID[s.byKey[key]]
	if !ok {
		return Entry{}, false, ErrNotFound
	}
	if err := CheckAnswers(en.Questions, answers); err != nil {
		return Entry{}, false, err
	}
	switch en.State {
	case Typing:
		if maps.Equal(en.Typed, answers) {
			return en.snapshot(), true, nil
		}
		return en.snapshot(), false, ErrBusy
	case Answered:
		if en.By == ByMuster && maps.Equal(en.Answers, answers) {
			return en.snapshot(), true, nil
		}
		return en.snapshot(), false, ErrClosed
	case Declined, Expired, Superseded:
		return en.snapshot(), false, ErrClosed
	case Failed:
		// 🔴 A HOST MAY STILL BE TYPING the last answer (its outcome never
		// came back): no second drive into the same picker until it is known
		// or MaxDrive has passed.
		if s.now().Before(en.driveUntil) {
			return en.snapshot(), false, ErrBusy
		}
	}
	// Left stays: it is what this drive finds in the picker (the caller hands
	// it to the host), until this drive's outcome replaces it.
	en.State, en.Typed, en.Note, en.Retry = Typing, maps.Clone(answers), "", false
	en.driveUntil = s.now().Add(MaxDrive)
	s.touch(en, &fire)
	return en.snapshot(), false, nil
}

// TypingDone records that the host reported it typed the answer in: the
// CLI's record will resolve the question; no drive is running, and nothing of
// muster's is left half-typed.
func (s *Store) TypingDone(key Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if en, ok := s.byID[s.byKey[key]]; ok {
		en.driveUntil, en.Left = time.Time{}, nil
	}
}

// Failure is how a drive that did not type the answer in ended.
type Failure struct {
	// Note is what the card says (Entry.Note).
	Note string
	// Settled says the host reported its outcome (it is not typing).
	Settled bool
	// Left is what the drive is known to have left in the picker (Entry.Left).
	// It is recorded only when Settled; nil (no key of this drive reached the
	// picker) keeps what the last drive left.
	Left *Progress
	// Retry says the question can be answered from muster again (Entry.Retry).
	Retry bool
}

// TypingFailed records that muster could not type the answer in (f): a
// Typing question becomes Failed, still the terminal's to answer. A question
// the CLI resolved meanwhile is left as it is. Unsettled, no new typing may
// begin until MaxDrive after the last began (BeginTyping's ErrBusy), and what
// the picker holds is not known (no Left).
func (s *Store) TypingFailed(key Key, f Failure) (Entry, error) {
	var fire []Entry
	defer func() { s.notify(fire) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	en, ok := s.byID[s.byKey[key]]
	if !ok {
		return Entry{}, ErrNotFound
	}
	if f.Settled {
		en.driveUntil = time.Time{}
		// A drive that typed nothing (refused before its first key: busy,
		// not sent, …) left the picker as the last one did: keep that.
		if f.Left != nil {
			en.Left = f.Left.clone()
		}
	} else {
		en.Left = nil
	}
	if en.State == Typing {
		en.State, en.Note, en.Retry = Failed, f.Note, f.Retry
		s.touch(en, &fire)
	}
	return en.snapshot(), nil
}

// Dismissing reports whether the question at key (key.Owner is the request's
// own owner; another owner's is ErrNotFound) may be dismissed from muster: an
// open question no answer is being typed into (ErrBusy for one that is, and
// for one whose last typing's outcome is not known yet — a host may still be
// typing it), and not resolved (ErrClosed).
func (s *Store) Dismissing(key Key) (Entry, error) {
	var fire []Entry
	defer func() { s.notify(fire) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(&fire)
	en, ok := s.byID[s.byKey[key]]
	switch {
	case !ok:
		return Entry{}, ErrNotFound
	case !en.State.Open():
		return en.snapshot(), ErrClosed
	case en.State == Typing, s.now().Before(en.driveUntil):
		return en.snapshot(), ErrBusy
	}
	return en.snapshot(), nil
}

// Gone expires every open question of (owner, source, scope, session) — the
// session is no longer running anywhere its host lists — with why as the
// Note. It returns how many it expired.
func (s *Store) Gone(owner int64, source, scope, session, why string) int {
	var fire []Entry
	defer func() { s.notify(fire) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.byID {
		if e.Key.Owner == owner && e.Key.Source == source && e.Key.Scope == scope && e.Key.Session == session && e.State.Open() {
			e.State, e.Note, e.Resolved = Expired, why, s.now()
			s.touch(e, &fire)
			n++
		}
	}
	return n
}

// Resolve records the call's result as the CLI recorded it: answers (nil when
// it ended with none — dismissed). An answered question is By muster when
// muster typed exactly these answers, else By the terminal. Resolving a
// resolved question changes nothing — except a Superseded or Expired one,
// whose result can still arrive (a session that came back, a late record) and
// is recorded. ok is false when the store has no open, superseded or expired
// question for the call.
func (s *Store) Resolve(owner int64, source, scope, session, toolUseID string, answers map[string]string) (Entry, bool) {
	var fire []Entry
	defer func() { s.notify(fire) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	key := Key{Owner: owner, Source: source, Scope: scope, Session: session, ToolUseID: toolUseID}
	en, ok := s.byID[s.byKey[key]]
	if !ok || !(en.State.Open() || en.State == Superseded || en.State == Expired) {
		return Entry{}, false
	}
	en.Resolved = s.now()
	if answers == nil {
		en.State = Declined
	} else {
		en.State, en.Answers, en.By = Answered, maps.Clone(answers), ByTerminal
		if en.Typed != nil && maps.Equal(en.Typed, answers) {
			en.By = ByMuster
		}
	}
	s.touch(en, &fire)
	return en.snapshot(), true
}

// Pending is owner's open questions, oldest first: what an attention view
// lists as "waiting on you".
func (s *Store) Pending(owner int64) []Entry {
	var fire []Entry
	defer func() { s.notify(fire) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(&fire)
	var out []Entry
	for _, e := range s.byID {
		if e.Key.Owner == owner && e.State.Open() {
			out = append(out, e.snapshot())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// OpenIn is the open questions of (owner, source, scope, session), oldest
// first: what a watcher of that session waits on.
func (s *Store) OpenIn(owner int64, source, scope, session string) []Entry {
	var out []Entry
	for _, e := range s.Pending(owner) {
		if e.Key.Source == source && e.Key.Scope == scope && e.Key.Session == session {
			out = append(out, e)
		}
	}
	return out
}

// Watched is the questions of (owner, source, scope, session) whose result
// can still be recorded: the open ones, and the superseded or expired ones
// still retained (Retain) — what a watcher of that session keeps reading the
// transcript for.
func (s *Store) Watched(owner int64, source, scope, session string) []Entry {
	var fire []Entry
	defer func() { s.notify(fire) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(&fire)
	var out []Entry
	for _, e := range s.byID {
		if e.Key.Owner == owner && e.Key.Source == source && e.Key.Scope == scope && e.Key.Session == session &&
			(e.State.Open() || e.State == Superseded || e.State == Expired) {
			out = append(out, e.snapshot())
		}
	}
	return out
}

// ForSession is owner's questions in (source, scope, session) that are open,
// or were resolved within the last shownFor — oldest first: what a view of
// that session shows.
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
		if e.State.Open() || now.Sub(e.Resolved) <= shownFor {
			out = append(out, e.snapshot())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// Sweep runs the expiry rules now, firing OnChange for every question it
// expires.
func (s *Store) Sweep() {
	var fire []Entry
	s.mu.Lock()
	s.sweepLocked(&fire)
	s.mu.Unlock()
	s.notify(fire)
}

// SweepEvery is RunSweep's period.
const SweepEvery = time.Minute

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
