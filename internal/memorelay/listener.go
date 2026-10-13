package memorelay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	// memosPath is cairn's `GET /client/v1/memos` (its `internal/memowire.MemosPath`).
	memosPath = "/client/v1/memos"

	// RefusedBackoff is how long the relay sends NOTHING after the listener refused its
	// credential (a 401).
	//
	// 🔴 IT IS THE LISTENER'S OWN LOCKOUT LENGTH, AND THE ARITHMETIC IS THE POINT. The listener
	// locks a peer out for 900 s after 5 refused credentials in 60 s, and behind a node's
	// masquerade every pod on that node — and the node's own host sessions — are ONE peer. With
	// this backoff the relay's refused requests are at least 900 s apart, so any 60 s window holds
	// at most one of them: it can never be what trips the lockout, and it probes a peer locked out
	// by something else at most once per lockout.
	RefusedBackoff = 15 * time.Minute

	// StartDelay is how long a new process sends nothing at all.
	//
	// 🔴 A NEWLY ISSUED CREDENTIAL IS REFUSED FOR UP TO 30 s, because the listener re-reads its
	// journal on a 30 s timer. A relay that asked the moment it started would be refused by a
	// correct credential and then sit out [RefusedBackoff]. It also bounds a crash loop: a process
	// that dies and restarts sends at most one request per StartDelay, however fast it restarts.
	StartDelay = time.Minute

	// UnreadableBackoff is how long a scope the listener answered its uniform miss for (404: no
	// such scope, or one this credential cannot read) is not asked for again. A 404 comes AFTER
	// authentication, so it counts toward no lockout; this only stops a request per tick for a
	// scope that will keep missing.
	UnreadableBackoff = 15 * time.Minute

	// requestTimeout bounds one exchange with the listener.
	requestTimeout = 10 * time.Second
	// maxResponseBytes bounds what is read of one answer. 200 live memos at cairn's send caps
	// measure well under 2 MiB; past it the answer is malformed, never truncated.
	maxResponseBytes = 4 << 20
)

// The closed set of ways asking the listener ends without memos. Each is counted under its own
// name ([Outcome]); none is retried inside a tick.
var (
	// ErrNotYet: inside [StartDelay]. No request was sent.
	ErrNotYet = errors.New("memorelay: inside the start delay; nothing was sent")
	// ErrBackedOff: the listener refused the credential earlier and [RefusedBackoff] has not
	// passed. No request was sent.
	ErrBackedOff = errors.New("memorelay: backed off after a refused credential; nothing was sent")
	// ErrRefused: the listener answered 401 to THIS request. [RefusedBackoff] starts now.
	ErrRefused = errors.New("memorelay: the listener refused the relay's credential (401)")
	// ErrScopeUnreadable: the listener's uniform miss for the scope (404), now or within
	// [UnreadableBackoff].
	ErrScopeUnreadable = errors.New("memorelay: the listener has no such scope for this credential (404)")
	// ErrUnreachable: no answer, or a status that is none of 200, 401, 404.
	ErrUnreachable = errors.New("memorelay: the listener could not be reached")
	// ErrMalformed: a 200 whose body is not the wire contract.
	ErrMalformed = errors.New("memorelay: the listener's answer is not a memo list")
)

// Memo is one live memo as the listener sent it: cairn's `memo.Rendered`, whose JSON keys are its
// Go field names. Every string is UNTRUSTED TEXT (see block.go).
type Memo struct {
	ID        int64
	Scope     string
	Sender    string
	CreatedAt string
	ExpiresAt string
	Subject   string
	Body      string
	Tombstone string

	// created is CreatedAt parsed; it orders a delivery (oldest first) and a block (newest first).
	created time.Time
}

// memoKeys is every field of cairn's `memo.Rendered`. All are REQUIRED of each memo, as cairn's
// own client requires them: a listener that stopped sending one is a malformed answer, not a memo
// with an empty field.
var memoKeys = []string{"ID", "Scope", "Sender", "CreatedAt", "ExpiresAt", "Subject", "Body", "Tombstone"}

// Listener is the relay's ONE way to the cairn memo listener, and the fleet's one lockout guard:
// every agent's memos are fetched through it, so there is one credential, one backoff and one
// request in flight for all of them.
type Listener struct {
	base   string
	token  string
	client *http.Client
	now    func() time.Time

	// mu is held across a WHOLE exchange, backoff read and write included: never more than one
	// upstream call in flight, and N callers that arrive together send at most one request
	// between them when the credential is refused (the rest read the backoff the first wrote).
	mu           sync.Mutex
	notBefore    time.Time
	refusedUntil time.Time
	unreadable   map[string]time.Time
}

// NewListener builds the guard for the listener at baseURL (`http://host:port`), authenticated by
// token. now is the clock (nil: time.Now); the start delay runs from this call.
func NewListener(baseURL, token string, now func() time.Time) (*Listener, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("memorelay: the listener URL must be http(s)://host[:port] with no path, " +
			"query, fragment or credentials")
	}
	if token == "" {
		return nil, errors.New("memorelay: a listener token is required")
	}
	if now == nil {
		now = time.Now
	}
	return &Listener{
		base:  u.Scheme + "://" + u.Host,
		token: token,
		now:   now,
		client: &http.Client{
			Timeout: requestTimeout,
			// 🔴 NO PROXY AND NO REDIRECTS: the request carries the credential. An environment
			// proxy would be handed it, and a redirect would carry it to wherever the answer
			// pointed. A 3xx is therefore an answer that is not 200, 401 or 404: unreachable.
			Transport:     &http.Transport{Proxy: nil, MaxIdleConns: 1, IdleConnTimeout: 30 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		notBefore:  now().Add(StartDelay),
		unreadable: map[string]time.Time{},
	}, nil
}

// Live returns scope's live memos — unexpired, unretracted, oldest first — or one of the errors
// above. It sends at most one request, and none at all inside the start delay, a refused-credential
// backoff or the scope's own miss backoff.
func (l *Listener) Live(ctx context.Context, scope string) ([]Memo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	switch {
	case now.Before(l.notBefore):
		return nil, ErrNotYet
	case now.Before(l.refusedUntil):
		return nil, ErrBackedOff
	case now.Before(l.unreadable[scope]):
		return nil, ErrScopeUnreadable
	}
	delete(l.unreadable, scope)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		l.base+memosPath+"?"+url.Values{"scope": {scope}}.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	req.Header.Set("Authorization", "Bearer "+l.token)
	resp, err := l.client.Do(req)
	if err != nil {
		// The error can quote the URL, never a header: the token is not in it.
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		l.refusedUntil = l.now().Add(RefusedBackoff)
		return nil, ErrRefused
	case http.StatusNotFound:
		l.unreadable[scope] = l.now().Add(UnreadableBackoff)
		return nil, ErrScopeUnreadable
	default:
		return nil, fmt.Errorf("%w: HTTP %d", ErrUnreachable, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading the answer: %v", ErrUnreachable, err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("%w: longer than %d bytes", ErrMalformed, maxResponseBytes)
	}
	return decodeList(body, scope)
}

// decodeList reads a `{"memos":[…]}` answer for scope. An unknown key is tolerated (the listener
// may add one in any release); a missing one, a memo of another scope, an id that is not positive
// or a time that does not parse makes the WHOLE answer malformed — nothing of it is delivered.
//
// 🔴 A TOMBSTONE IS DROPPED HERE, NOT TRUSTED TO BE ABSENT. This request never asks for
// retracted memos, so a real listener sends none; one that did would otherwise have its
// retraction notice delivered as a memo.
func decodeList(body []byte, scope string) ([]Memo, error) {
	var answer struct {
		Memos *[]map[string]json.RawMessage `json:"memos"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("%w: not JSON", ErrMalformed)
	}
	if answer.Memos == nil {
		return nil, fmt.Errorf("%w: no `memos` key", ErrMalformed)
	}
	out := make([]Memo, 0, len(*answer.Memos))
	seen := map[int64]bool{}
	for _, raw := range *answer.Memos {
		for _, k := range memoKeys {
			if v, ok := raw[k]; !ok || string(v) == "null" {
				return nil, fmt.Errorf("%w: a memo has no `%s`", ErrMalformed, k)
			}
		}
		// Re-encode the one memo and decode it into the typed shape, so field matching is the
		// decoder's own and the required-key check above is the only list of names.
		one, _ := json.Marshal(raw)
		var m Memo
		if err := json.Unmarshal(one, &m); err != nil {
			return nil, fmt.Errorf("%w: a memo's fields have the wrong types", ErrMalformed)
		}
		created, err := time.Parse(time.RFC3339, m.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("%w: memo %d's created time is not RFC 3339", ErrMalformed, m.ID)
		}
		if _, err := time.Parse(time.DateOnly, m.ExpiresAt); err != nil {
			return nil, fmt.Errorf("%w: memo %d's expiry is not a date", ErrMalformed, m.ID)
		}
		switch {
		case m.ID <= 0:
			return nil, fmt.Errorf("%w: a memo's id is not positive", ErrMalformed)
		case m.Scope != scope:
			return nil, fmt.Errorf("%w: memo %d belongs to another scope than the one asked for", ErrMalformed, m.ID)
		case seen[m.ID]:
			return nil, fmt.Errorf("%w: memo %d is listed twice", ErrMalformed, m.ID)
		}
		seen[m.ID] = true
		if m.Tombstone != "" {
			continue
		}
		m.created = created
		out = append(out, m)
	}
	return out, nil
}
