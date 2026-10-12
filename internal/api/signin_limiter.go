package api

import (
	"container/list"
	"sync"
	"time"
)

// signinLimiter is gotrue mode's sign-in throttle: an exponential backoff per
// ACCOUNT (the real key) and a fixed window per forwarded client address (the
// secondary). See the constants' comment in login_gotrue.go for why the key
// is the account and why there is no process-wide ceiling.
//
// 🔴 RESERVE, THEN CALL GoTrue — NEVER CHECK, CALL, THEN RECORD. The first
// version checked the limits, called GoTrue, and recorded the failure only
// when the answer came back. Nothing was held in flight, so 200 PARALLEL
// wrong-password attempts at one account all passed the check before any had
// failed: an audit measured 136, 159 and 179 of 200 reaching GoTrue (4 when
// sent one at a time). Now reserve:
//
//   - counts the attempt as a FAILURE up front, against the account and the
//     address, before GoTrue is asked; settle refunds it on success (and on an
//     answer that judged nothing — GoTrue down or rate-limiting);
//   - allows at most ONE attempt in flight per account: a concurrent attempt
//     for the same account gets the backoff answer.
//
// So the number of GoTrue calls for one account is bounded by the backoff
// schedule however the requests are timed (TestParallelSignInsAtOneAccountAreBounded).
//
// ⚠ ITS OWN MUTEX, AND BOUNDED. It is not gotrueState.mu, which every
// authenticated request takes; and each map is capped at signinLimiterCap
// entries. Address records are evicted least-recently-touched (the address is
// the secondary, forgeable key). Account records are evicted only when they
// are neither in flight nor backing off — a full table of protected records
// refuses a new account instead (see accountRecordLocked) — so churning other
// accounts cannot reset a victim's backoff.
type signinLimiter struct {
	mu       sync.Mutex
	accounts map[string]*list.Element // value: *accountRecord
	accOrder *list.List               // front = most recently touched
	ips      map[string]*list.Element // value: *ipRecord
	ipOrder  *list.List
	gen      uint64 // the next account record's generation
}

// evictScan is how many of the least recently touched account records
// accountRecordLocked examines for one it may evict.
const evictScan = 256

// signinLimiterCap bounds each of the limiter's maps.
const signinLimiterCap = 10000

type accountRecord struct {
	key string
	// gen identifies THIS record: an evicted-and-recreated account gets a new
	// one, so a ticket from the old record cannot settle the new record.
	gen      uint64
	failures int
	until    time.Time
	last     time.Time
	inFlight bool
}

type ipRecord struct {
	key   string
	start time.Time
	n     int
}

// signinTicket is one reserved attempt; settle it exactly once.
type signinTicket struct {
	account, ip   string
	gen           uint64
	prevFailures  int
	prevUntil     time.Time
	accountCounts bool
}

// signinOutcome is how an attempt ended.
type signinOutcome int

const (
	// outcomeFailed: the credential was judged wrong. The reservation stands.
	outcomeFailed signinOutcome = iota
	// outcomeSucceeded: signed in. The account's record is cleared and the
	// address's reservation refunded.
	outcomeSucceeded
	// outcomeUnjudged: nothing was judged (GoTrue down, rate-limited, a
	// form refused before GoTrue). Both reservations are refunded.
	outcomeUnjudged
)

func (l *signinLimiter) init() {
	if l.accounts == nil {
		l.accounts, l.accOrder = map[string]*list.Element{}, list.New()
		l.ips, l.ipOrder = map[string]*list.Element{}, list.New()
	}
}

// backoffFor is the wait after failures failures.
func backoffFor(failures int) time.Duration {
	over := failures - accountFreeFailures
	if over <= 0 {
		return 0
	}
	if over > 20 {
		return accountBackoffMax
	}
	if d := accountBackoffBase << (over - 1); d < accountBackoffMax {
		return d
	}
	return accountBackoffMax
}

// reserve claims one attempt for account (may be "" — the claim form, limited
// by address only) from ip at now. ok is false when it must wait.
func (l *signinLimiter) reserve(account, ip string, now time.Time) (signinTicket, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	ipr := l.ipRecordLocked(ip, now)
	if ipr.n >= loginFailureLimit {
		return signinTicket{}, false
	}
	t := signinTicket{account: account, ip: ip}
	if account != "" {
		ar := l.accountRecordLocked(account, now)
		if ar == nil || ar.inFlight || now.Before(ar.until) {
			return signinTicket{}, false
		}
		t.accountCounts = true
		t.gen = ar.gen
		t.prevFailures, t.prevUntil = ar.failures, ar.until
		ar.inFlight = true
		ar.failures++
		ar.last = now
		if d := backoffFor(ar.failures); d > 0 {
			ar.until = now.Add(d)
		}
	}
	ipr.n++
	return t, true
}

// settle ends a reserved attempt.
func (l *signinLimiter) settle(t signinTicket, outcome signinOutcome) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	if outcome != outcomeFailed {
		if e, ok := l.ips[t.ip]; ok {
			if r := e.Value.(*ipRecord); r.n > 0 {
				r.n--
			}
		}
	}
	if !t.accountCounts {
		return
	}
	e, ok := l.accounts[t.account]
	if !ok {
		return // evicted while in flight: nothing to settle
	}
	ar := e.Value.(*accountRecord)
	if ar.gen != t.gen {
		return // the record this ticket reserved was evicted; this is another
	}
	ar.inFlight = false
	switch outcome {
	case outcomeSucceeded:
		l.accOrder.Remove(e)
		delete(l.accounts, t.account)
	case outcomeUnjudged:
		ar.failures, ar.until = t.prevFailures, t.prevUntil
	}
}

// accountRecordLocked returns account's record, creating it when absent. A
// record idle for an hour starts over. It returns nil — the caller answers
// the backoff 429 — when the table is full and nothing may be evicted.
//
// 🔴 EVICTION NEVER TAKES A PROTECTED RECORD. The first version evicted the
// least recently touched record unconditionally, so 10,001 sign-ins at
// made-up emails (from forged addresses) pushed a victim's backoff out of the
// table and reset it: 4 more guesses at the victim got through. Now only a
// record that is neither in flight nor still backing off (until <= now) may go;
// if none of the evictScan oldest qualifies, the NEW key is refused instead —
// fail closed (TestChurnCannotEvictAVictimsBackoff). The cost, stated: a flood
// that keeps signinLimiterCap accounts in backoff makes sign-in answer 429 for
// accounts with no record yet, for as long as the flood keeps them backing off.
func (l *signinLimiter) accountRecordLocked(account string, now time.Time) *accountRecord {
	if e, ok := l.accounts[account]; ok {
		l.accOrder.MoveToFront(e)
		ar := e.Value.(*accountRecord)
		if !ar.inFlight && now.Sub(ar.last) > time.Hour {
			ar.failures, ar.until = 0, time.Time{}
		}
		return ar
	}
	if len(l.accounts) >= signinLimiterCap {
		var victim *list.Element
		for e, i := l.accOrder.Back(), 0; e != nil && i < evictScan; e, i = e.Prev(), i+1 {
			if r := e.Value.(*accountRecord); !r.inFlight && !now.Before(r.until) {
				victim = e
				break
			}
		}
		if victim == nil {
			return nil
		}
		l.accOrder.Remove(victim)
		delete(l.accounts, victim.Value.(*accountRecord).key)
	}
	l.gen++
	ar := &accountRecord{key: account, last: now, gen: l.gen}
	l.accounts[account] = l.accOrder.PushFront(ar)
	return ar
}

// ipRecordLocked returns ip's window, resetting it once the window passed.
func (l *signinLimiter) ipRecordLocked(ip string, now time.Time) *ipRecord {
	if e, ok := l.ips[ip]; ok {
		l.ipOrder.MoveToFront(e)
		r := e.Value.(*ipRecord)
		if now.Sub(r.start) >= loginFailureWindow {
			r.start, r.n = now, 0
		}
		return r
	}
	r := &ipRecord{key: ip, start: now}
	l.ips[ip] = l.ipOrder.PushFront(r)
	for len(l.ips) > signinLimiterCap {
		oldest := l.ipOrder.Back()
		l.ipOrder.Remove(oldest)
		delete(l.ips, oldest.Value.(*ipRecord).key)
	}
	return r
}

// sizes reports the two maps' sizes (tests).
func (l *signinLimiter) sizes() (accounts, ips int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.accounts), len(l.ips)
}
