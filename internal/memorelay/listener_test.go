package memorelay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a settable clock; every Listener in these tests reads it.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// stub is a memo listener: it counts what reaches it, the most requests it ever held at once, and
// answers whatever `answer` says for the scope asked.
type stub struct {
	srv         *httptest.Server
	requests    atomic.Int64
	inFlight    atomic.Int64
	maxInFlight atomic.Int64
	hold        time.Duration

	mu     sync.Mutex
	seen   []*http.Request
	answer func(scope string) (status int, body string)
}

func newStub(t *testing.T, answer func(scope string) (int, string)) *stub {
	t.Helper()
	s := &stub{answer: answer}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		now := s.inFlight.Add(1)
		defer s.inFlight.Add(-1)
		for {
			max := s.maxInFlight.Load()
			if now <= max || s.maxInFlight.CompareAndSwap(max, now) {
				break
			}
		}
		if s.hold > 0 {
			time.Sleep(s.hold)
		}
		s.mu.Lock()
		s.seen = append(s.seen, r.Clone(context.Background()))
		answer := s.answer
		s.mu.Unlock()
		status, body := answer(r.URL.Query().Get("scope"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stub) setAnswer(answer func(scope string) (int, string)) {
	s.mu.Lock()
	s.answer = answer
	s.mu.Unlock()
}

// started builds a Listener on the stub whose start delay has already passed.
func started(t *testing.T, s *stub, c *clock) *Listener {
	t.Helper()
	l, err := NewListener(s.srv.URL, "tok-fixture", c.now)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(StartDelay)
	return l
}

const emptyList = `{"memos":[]}`

func oneMemoJSON(id, scope string) string {
	return `{"memos":[{"ID":` + id + `,"Scope":"` + scope + `","Sender":"Ada via laptop (user)",` +
		`"CreatedAt":"2000-01-02T03:04:05Z","ExpiresAt":"2000-01-09","Subject":"s","Body":"b","Tombstone":""}]}`
}

// TestNothingIsAskedInsideTheStartDelay: a credential issued moments ago is refused for up to
// 30 s, so a process that asked at once would be refused by a CORRECT credential. Measured at the
// start, one nanosecond short of the delay, and at it.
func TestNothingIsAskedInsideTheStartDelay(t *testing.T) {
	s := newStub(t, func(string) (int, string) { return 200, emptyList })
	c := newClock()
	l, err := NewListener(s.srv.URL, "tok-fixture", c.now)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []time.Duration{0, StartDelay - time.Nanosecond} {
		c.advance(step)
		if _, err := l.Live(context.Background(), "widgets"); !errors.Is(err, ErrNotYet) {
			t.Fatalf("inside the start delay: err = %v, want ErrNotYet", err)
		}
	}
	if n := s.requests.Load(); n != 0 {
		t.Fatalf("%d request(s) reached the listener inside the start delay, want 0", n)
	}
	c.advance(time.Nanosecond)
	if _, err := l.Live(context.Background(), "widgets"); err != nil {
		t.Fatalf("at the end of the start delay: %v", err)
	}
	if n := s.requests.Load(); n != 1 {
		t.Fatalf("positive control: %d request(s) after the start delay, want 1", n)
	}
	if StartDelay < 30*time.Second {
		t.Fatalf("StartDelay is %s; the listener re-reads its journal every 30 s, so anything shorter "+
			"can ask before a new credential exists", StartDelay)
	}
}

// TestARefusedCredentialSilencesEveryScopeForTheWholeBackoff is the fleet's lockout guard: after
// ONE 401, nothing is sent — for the refused scope or any other — until RefusedBackoff has
// passed. Measured one second in, one nanosecond short of the end, and at the end.
func TestARefusedCredentialSilencesEveryScopeForTheWholeBackoff(t *testing.T) {
	s := newStub(t, func(string) (int, string) { return 401, `{"error":"unauthorized"}` })
	c := newClock()
	l := started(t, s, c)

	if _, err := l.Live(context.Background(), "widgets"); !errors.Is(err, ErrRefused) {
		t.Fatalf("the refused call: err = %v, want ErrRefused", err)
	}
	if n := s.requests.Load(); n != 1 {
		t.Fatalf("positive control: the refused call sent %d request(s), want 1", n)
	}
	// Even a listener that would now answer 200 is not asked.
	s.setAnswer(func(string) (int, string) { return 200, emptyList })
	for _, step := range []time.Duration{time.Second, RefusedBackoff - time.Second - time.Nanosecond} {
		c.advance(step)
		for _, scope := range []string{"widgets", "gadgets"} {
			if _, err := l.Live(context.Background(), scope); !errors.Is(err, ErrBackedOff) {
				t.Fatalf("inside the backoff, scope %s: err = %v, want ErrBackedOff", scope, err)
			}
		}
	}
	if n := s.requests.Load(); n != 1 {
		t.Fatalf("%d request(s) reached the listener in all, want exactly the 1 that was refused", n)
	}
	c.advance(time.Nanosecond)
	if _, err := l.Live(context.Background(), "widgets"); err != nil {
		t.Fatalf("once the backoff has passed: %v", err)
	}
	if n := s.requests.Load(); n != 2 {
		t.Fatalf("after the backoff: %d request(s) in all, want 2", n)
	}
	if RefusedBackoff < 15*time.Minute {
		t.Fatalf("RefusedBackoff is %s; the listener's lockout is 15 minutes", RefusedBackoff)
	}
}

// TestCallersThatArriveTogetherSendOneRefusedRequestBetweenThem: the listener locks a peer out at
// FIVE refused credentials in a minute, so a burst must not spend more than one.
func TestCallersThatArriveTogetherSendOneRefusedRequestBetweenThem(t *testing.T) {
	s := newStub(t, func(string) (int, string) { return 401, `{}` })
	s.hold = 20 * time.Millisecond
	c := newClock()
	l := started(t, s, c)

	const callers = 12
	var wg sync.WaitGroup
	var refused, backedOff atomic.Int64
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := l.Live(context.Background(), []string{"widgets", "gadgets", "sprockets"}[i%3])
			switch {
			case errors.Is(err, ErrRefused):
				refused.Add(1)
			case errors.Is(err, ErrBackedOff):
				backedOff.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if n := s.requests.Load(); n != 1 {
		t.Fatalf("%d request(s) reached the listener from %d concurrent callers, want 1", n, callers)
	}
	if refused.Load() != 1 || backedOff.Load() != callers-1 {
		t.Fatalf("refused=%d backed-off=%d, want 1 and %d", refused.Load(), backedOff.Load(), callers-1)
	}
}

// TestNeverMoreThanOneCallIsInFlight: with a healthy listener every caller is answered, one at a
// time. The positive control is the count — all of them did reach the listener.
func TestNeverMoreThanOneCallIsInFlight(t *testing.T) {
	s := newStub(t, func(string) (int, string) { return 200, emptyList })
	s.hold = 10 * time.Millisecond
	c := newClock()
	l := started(t, s, c)

	const callers = 8
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Live(context.Background(), "widgets"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := s.requests.Load(); n != callers {
		t.Fatalf("positive control: %d request(s) reached the listener, want %d", n, callers)
	}
	if m := s.maxInFlight.Load(); m != 1 {
		t.Fatalf("the listener held %d requests at once, want never more than 1", m)
	}
}

// TestAMissedScopeIsNotAskedAgainButOtherScopesAre: a 404 is the listener's uniform miss, sent
// after authentication — it backs off that scope and nothing else.
func TestAMissedScopeIsNotAskedAgainButOtherScopesAre(t *testing.T) {
	s := newStub(t, func(scope string) (int, string) {
		if scope == "gone" {
			return 404, `{"error":"not-found"}`
		}
		return 200, emptyList
	})
	c := newClock()
	l := started(t, s, c)

	for i := 0; i < 3; i++ {
		if _, err := l.Live(context.Background(), "gone"); !errors.Is(err, ErrScopeUnreadable) {
			t.Fatalf("call %d for the missed scope: err = %v, want ErrScopeUnreadable", i, err)
		}
	}
	if n := s.requests.Load(); n != 1 {
		t.Fatalf("the missed scope was asked for %d time(s), want 1", n)
	}
	if _, err := l.Live(context.Background(), "widgets"); err != nil {
		t.Fatalf("another scope must still be asked: %v", err)
	}
	if n := s.requests.Load(); n != 2 {
		t.Fatalf("%d request(s) in all, want 2 (a miss must not silence other scopes)", n)
	}
	c.advance(UnreadableBackoff)
	_, _ = l.Live(context.Background(), "gone")
	if n := s.requests.Load(); n != 3 {
		t.Fatalf("after its backoff the missed scope is asked again: %d request(s), want 3", n)
	}
}

// TestTheRequestIsTheWireContract pins what is sent: cairn's path, the scope as the one query
// parameter (so retracted memos are never asked for), and the credential as a bearer.
func TestTheRequestIsTheWireContract(t *testing.T) {
	s := newStub(t, func(string) (int, string) { return 200, emptyList })
	c := newClock()
	l := started(t, s, c)
	if _, err := l.Live(context.Background(), "wid gets&x=1"); err != nil {
		t.Fatal(err)
	}
	r := s.seen[0]
	if r.Method != http.MethodGet || r.URL.Path != "/client/v1/memos" {
		t.Errorf("request = %s %s, want GET /client/v1/memos", r.Method, r.URL.Path)
	}
	if got := r.URL.Query(); len(got) != 1 || got.Get("scope") != "wid gets&x=1" {
		t.Errorf("query = %v, want exactly scope=<the scope, encoded>", got)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer tok-fixture" {
		t.Errorf("Authorization = %q, want the bearer", got)
	}
}

func TestALiveMemoIsDecodedAndATombstoneIsDropped(t *testing.T) {
	body := `{"memos":[
		{"ID":4,"Scope":"widgets","Sender":"Ada","CreatedAt":"2000-01-02T03:04:05Z","ExpiresAt":"2000-01-09",
		 "Subject":"s","Body":"b","Tombstone":"","FieldFromALaterRelease":true},
		{"ID":5,"Scope":"widgets","Sender":"Ada","CreatedAt":"2000-01-02T04:04:05Z","ExpiresAt":"2000-01-09",
		 "Subject":"","Body":"","Tombstone":"retracted by Ada (user) at 2000-01-02T05:00:00Z"}
	],"next":"a top-level key from a later release"}`
	s := newStub(t, func(string) (int, string) { return 200, body })
	c := newClock()
	l := started(t, s, c)
	got, err := l.Live(context.Background(), "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 4 || got[0].Subject != "s" || got[0].Body != "b" || got[0].created.IsZero() {
		t.Fatalf("decoded %+v, want exactly memo 4 with its fields (the tombstone dropped, the unknown keys ignored)", got)
	}
}

// TestAnAnswerThatIsNotTheContractDeliversNothing: each defect makes the WHOLE answer malformed,
// including its well-formed memos.
func TestAnAnswerThatIsNotTheContractDeliversNothing(t *testing.T) {
	good := `{"ID":4,"Scope":"widgets","Sender":"Ada","CreatedAt":"2000-01-02T03:04:05Z","ExpiresAt":"2000-01-09","Subject":"s","Body":"b","Tombstone":""}`
	for name, body := range map[string]string{
		"not JSON":             `<html>`,
		"no memos key":         `{"items":[]}`,
		"null memos":           `{"memos":null}`,
		"a missing field":      `{"memos":[` + good + `,{"ID":5,"Scope":"widgets","Sender":"Ada","CreatedAt":"2000-01-02T03:04:05Z","ExpiresAt":"2000-01-09","Subject":"s","Body":"b"}]}`,
		"a null field":         `{"memos":[` + strings.Replace(good, `"Body":"b"`, `"Body":null`, 1) + `]}`,
		"a wrongly typed id":   `{"memos":[` + strings.Replace(good, `"ID":4`, `"ID":"4"`, 1) + `]}`,
		"a non-positive id":    `{"memos":[` + strings.Replace(good, `"ID":4`, `"ID":0`, 1) + `]}`,
		"another scope's memo": `{"memos":[` + good + `,` + strings.Replace(strings.Replace(good, `"ID":4`, `"ID":5`, 1), `"Scope":"widgets"`, `"Scope":"gadgets"`, 1) + `]}`,
		"a bad created time":   `{"memos":[` + strings.Replace(good, `2000-01-02T03:04:05Z`, `yesterday`, 1) + `]}`,
		"a bad expiry":         `{"memos":[` + strings.Replace(good, `"ExpiresAt":"2000-01-09"`, `"ExpiresAt":"soon"`, 1) + `]}`,
		"a memo listed twice":  `{"memos":[` + good + `,` + good + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newStub(t, func(string) (int, string) { return 200, body })
			c := newClock()
			l := started(t, s, c)
			got, err := l.Live(context.Background(), "widgets")
			if !errors.Is(err, ErrMalformed) || got != nil {
				t.Fatalf("got %d memo(s), err = %v; want none and ErrMalformed", len(got), err)
			}
		})
	}
	// Positive control: the well-formed memo alone IS accepted, so the cases above are refused
	// for their defect and not for the fixture.
	s := newStub(t, func(string) (int, string) { return 200, `{"memos":[` + good + `]}` })
	c := newClock()
	if got, err := started(t, s, c).Live(context.Background(), "widgets"); err != nil || len(got) != 1 {
		t.Fatalf("positive control: got %d memo(s), err = %v; want 1 and nil", len(got), err)
	}
}

// TestARedirectIsNotFollowed: the request carries the credential, and a redirect would carry it
// to wherever the answer pointed.
func TestARedirectIsNotFollowed(t *testing.T) {
	var elsewhere atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		_, _ = w.Write([]byte(emptyList))
	}))
	defer other.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	defer s.Close()
	c := newClock()
	l, err := NewListener(s.URL, "tok-fixture", c.now)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(StartDelay)
	if _, err := l.Live(context.Background(), "widgets"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("a redirect: err = %v, want ErrUnreachable", err)
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("the redirect was followed %d time(s): the credential left for another host", n)
	}
}

// TestAServerErrorBacksOffNobody: a 5xx says nothing about the credential, so the next call asks
// again.
func TestAServerErrorBacksOffNobody(t *testing.T) {
	s := newStub(t, func(string) (int, string) { return 500, `{"error":"internal"}` })
	c := newClock()
	l := started(t, s, c)
	for i := 0; i < 2; i++ {
		if _, err := l.Live(context.Background(), "widgets"); !errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable", err)
		}
	}
	if n := s.requests.Load(); n != 2 {
		t.Fatalf("%d request(s), want 2", n)
	}
}

func TestTheListenerURLMustBeABareBase(t *testing.T) {
	for _, bad := range []string{"", "host:8125", "ftp://host", "http://", "http://user:pw@host", "http://host/base",
		"http://host?x=1", "http://host#f"} {
		if _, err := NewListener(bad, "tok-fixture", nil); err == nil {
			t.Errorf("NewListener(%q) was accepted", bad)
		}
	}
	if _, err := NewListener("http://host.example.invalid:8125", "", nil); err == nil {
		t.Error("an empty token was accepted")
	}
	for _, good := range []string{"http://host.example.invalid:8125", "https://host.example.invalid/"} {
		if _, err := NewListener(good, "tok-fixture", nil); err != nil {
			t.Errorf("NewListener(%q): %v", good, err)
		}
	}
}
