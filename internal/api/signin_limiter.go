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
// 🔴 RESERVE, THEN CALL GoTrue — NEVER CHECK, CALL, THEN RECORD. reserve
// counts the attempt as a FAILURE up front, against the account and the
// address, before GoTrue is asked; settle refunds it on success (and on an
// answer that judged nothing — GoTrue down or rate-limiting). At most ONE
// attempt per account is in flight. So the number of GoTrue calls for one
// account is bounded by the backoff schedule however the requests are timed
// (TestParallelSignInsAtOneAccountAreBounded; check-then-record let 136-179 of
// 200 parallel attempts through).
//
// 🔴 TWO ACCOUNT TABLES, AND ONLY ONE OF THEM CAN BE FULL. A single capped
// table cannot be right: evicting records lets churn reset a victim's backoff,
// and refusing new records when full locks out everyone without one — a
// successful sign-in clears its record, so that was the operator (measured:
// 256 records in backoff plus filler made the operator's CORRECT sign-in 429).
// So the account is resolved first (users.IDByEmail):
//
//   - a KNOWN account — an email that is a muster user — is keyed by user id
//     in `known`, which is bounded by the number of users, never evicted and
//     never full: a known account is never refused for capacity, and churn
//     cannot reset its backoff (TestAKnownAccountSurvivesAnyFlood);
//   - an UNKNOWN email goes to `unknown`, a least-recently-touched table
//     capped at unknownCap that evicts freely and never fails closed —
//     guessing a password for an account that does not exist gains nothing.
//
// ⚠ THE TRADE-OFF, CHOSEN AND STATED: under a flood big enough to evict
// unknown records, an unknown email's backoff can be forgotten while a known
// account's never is — so a patient attacker can tell which emails are muster
// users by which keep answering 429. The answers are otherwise identical
// (same 401 text, same 429 text). Equalising it would mean keeping unbounded
// state for invented emails, which is the lever this design removes. An
// email-existence oracle under a sustained flood is the accepted cost.
//
// ⚠ ITS OWN MUTEX, not gotrueState.mu (which every authenticated request
// takes). Address records are evicted least-recently-touched at signinIPCap
// (the address is the forgeable secondary key).
type signinLimiter struct {
	mu       sync.Mutex
	known    map[int64]*accountRecord
	unknown  map[string]*list.Element // value: *accountRecord
	unkOrder *list.List               // front = most recently touched
	ips      map[string]*list.Element // value: *ipRecord
	ipOrder  *list.List
	gen      uint64
	// unknownCap is the unknown table's cap; 0 means signinLimiterCap. A
	// field so a test can fill it without 10,000 requests.
	unknownCap int
}

// signinLimiterCap bounds the unknown-email and address tables.
const signinLimiterCap = 10000

// signinIPCap bounds the address table.
const signinIPCap = signinLimiterCap

// signinAccount names whose attempt it is: a known user (by id), an unknown
// email, or nobody (the claim form, limited by address only).
type signinAccount struct {
	known bool
	uid   int64
	email string
}

func (a signinAccount) none() bool { return !a.known && a.email == "" }

type accountRecord struct {
	email string // unknown table only
	// gen identifies this record. ⚠ DEFENSIVE: a ticket can only outlive its
	// record if an IN-FLIGHT unknown record is evicted, and eviction skips
	// in-flight records while any other is evictable; the check makes a stale
	// settle a no-op rather than relying on that.
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
	acct          signinAccount
	ip            string
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
	// outcomeSucceeded: signed in. The account's failures are cleared and
	// the address's reservation refunded.
	outcomeSucceeded
	// outcomeUnjudged: nothing was judged (GoTrue down, rate-limited, a
	// form refused before GoTrue). Both reservations are refunded.
	outcomeUnjudged
)

func (l *signinLimiter) init() {
	if l.known == nil {
		l.known = map[int64]*accountRecord{}
		l.unknown, l.unkOrder = map[string]*list.Element{}, list.New()
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

// reserve claims one attempt for acct from ip at now. ok is false when it
// must wait.
func (l *signinLimiter) reserve(acct signinAccount, ip string, now time.Time) (signinTicket, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	ipr := l.ipRecordLocked(ip, now)
	if ipr.n >= loginFailureLimit {
		return signinTicket{}, false
	}
	t := signinTicket{acct: acct, ip: ip}
	if !acct.none() {
		ar := l.accountRecordLocked(acct, now)
		if ar.inFlight || now.Before(ar.until) {
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
	ar := l.lookupLocked(t.acct)
	if ar == nil || ar.gen != t.gen {
		return // defensive: see accountRecord.gen
	}
	ar.inFlight = false
	switch outcome {
	case outcomeSucceeded:
		ar.failures, ar.until = 0, time.Time{}
	case outcomeUnjudged:
		ar.failures, ar.until = t.prevFailures, t.prevUntil
	}
}

func (l *signinLimiter) lookupLocked(acct signinAccount) *accountRecord {
	if acct.known {
		return l.known[acct.uid]
	}
	if e, ok := l.unknown[acct.email]; ok {
		return e.Value.(*accountRecord)
	}
	return nil
}

// accountRecordLocked returns acct's record, creating it when absent. A
// record idle for an hour starts over. It never returns nil: the known table
// has no cap, and the unknown table evicts.
func (l *signinLimiter) accountRecordLocked(acct signinAccount, now time.Time) *accountRecord {
	ar := l.lookupLocked(acct)
	if ar != nil {
		if !acct.known {
			l.unkOrder.MoveToFront(l.unknown[acct.email])
		}
		if !ar.inFlight && now.Sub(ar.last) > time.Hour {
			ar.failures, ar.until = 0, time.Time{}
		}
		return ar
	}
	l.gen++
	ar = &accountRecord{email: acct.email, last: now, gen: l.gen}
	if acct.known {
		l.known[acct.uid] = ar
		return ar
	}
	l.unknown[acct.email] = l.unkOrder.PushFront(ar)
	limit := l.unknownCap
	if limit <= 0 {
		limit = signinLimiterCap
	}
	for len(l.unknown) > limit {
		// Prefer a record not in flight; an in-flight one goes only when
		// every other is in flight too (its settle is then a no-op).
		victim := l.unkOrder.Back()
		for e := victim; e != nil; e = e.Prev() {
			if !e.Value.(*accountRecord).inFlight {
				victim = e
				break
			}
		}
		l.unkOrder.Remove(victim)
		delete(l.unknown, victim.Value.(*accountRecord).email)
	}
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
	for len(l.ips) > signinIPCap {
		oldest := l.ipOrder.Back()
		l.ipOrder.Remove(oldest)
		delete(l.ips, oldest.Value.(*ipRecord).key)
	}
	return r
}

// sizes reports the tables' sizes (tests).
func (l *signinLimiter) sizes() (known, unknown, ips int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.known), len(l.unknown), len(l.ips)
}
