// Package ccpool is the per-account Claude Code `setup-token` pool: which
// operator account a new claude-code agent runs on, and what each account's
// sessions have reported back.
//
// # The shape
//
// The operator has several INDIVIDUAL Pro/Max accounts and runs `claude
// setup-token` once per account. Each token is one value in muster's own
// environment (MUSTER_AGENT_CC_TOKEN_<NAME>, named by MUSTER_AGENT_CC_ACCOUNTS —
// see cmd/muster-server/config.go). This package holds that map and answers
// three questions:
//
//   - Select: which account should a NEW claude-code agent use?
//   - Token: which setup-token does a pod on account X get?
//   - MarkFailure: ccd reported `rate_limited` or `auth_failed` for an agent on
//     account X — remember it, with a timestamp, in Postgres.
//
// # Whose accounts (bring-your-own, migration 0011)
//
// The environment pool is the ADMIN's. A member's agents run only on accounts
// the member entered in muster (internal/credentials). The [Source] answers
// "which accounts may this OWNER use", and every selection, mark and live count
// is per (owner, account): there is no path from a member's dispatch to an
// environment-pool account (TestAMemberDispatchNeverSelectsAnAdminPoolAccount
// in internal/credentials).
//
// # Selection
//
// [Choose] is the whole algorithm, and it is a pure function so a test can pin
// it without a database:
//
//  1. Drop every account whose CURRENT token is marked auth_failed (the mark's
//     fingerprint equals [Fingerprint] of the token configured now). Replacing
//     the token in the secret therefore clears the mark by itself; nothing else
//     does. If that leaves nothing, refuse — dispatching onto a token that
//     already failed authentication builds a pod that cannot answer one turn.
//  2. Least-recently-rate-limited first: never rate-limited sorts before any
//     timestamp, then oldest timestamp first.
//  3. Then fewest LIVE agents (kind claude-code, status pending, provisioning or
//     running) on the account.
//  4. Then the account name, so the answer is deterministic.
//
// An explicit pin at dispatch bypasses all four — it is the operator's override,
// including for an account the pool would skip — but must name a configured
// account.
//
// 🔴 AN AGENT KEEPS ITS ACCOUNT FOR LIFE. Select runs once, before the agent's
// row exists; the row stores the NAME (agents.cc_account) and migration 0003
// refuses any later change. A restart, a re-provision, a model of the pool
// changing under it — none of them re-select.
//
// 🔴 [Pool.Token] IS THE ONE PLACE A TOKEN LEAVES THIS PACKAGE: the pod-spec
// path (internal/agentprovision, through internal/credentials.Plane) asks it
// for the token of the agent's OWN owner and account, and agentspec places it
// in exactly one place — the agent's own per-agent Secret, as
// CLAUDE_CODE_OAUTH_TOKEN. No error, log line or stored mark here carries it;
// a mark stores a truncated sha256 fingerprint.
//
// ⚠ ROTATING A TOKEN DOES NOT REACH A RUNNING POD. The new value is in muster's
// environment; an existing agent's Secret still holds the old one until its spec
// is re-applied — Stop then Start a claude-code agent does that
// (agentprovision.Adapter.Start). Until then its turns keep failing and keep
// re-marking the account, now under the NEW token's fingerprint.
package ccpool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// The failure types ccd reports that mark an account. They are ccd's own
// spellings (cmd/ccd/failure.go: failRateLimited / failAuthFailed) — the wire's
// `error.type` — and nothing else marks.
const (
	FailureRateLimited = "rate_limited"
	FailureAuthFailed  = "auth_failed"
)

// ErrNoUsableAccount is returned by Select when automatic selection has nothing
// to choose from. It is a dispatch REFUSAL, surfaced to the operator.
var ErrNoUsableAccount = errors.New("ccpool: no usable Claude account")

// ErrUnknownAccount is returned for a pin (or a lookup) naming an account that
// is not configured.
var ErrUnknownAccount = errors.New("ccpool: unknown Claude account")

// maxNameLen bounds an account name. It becomes part of an environment variable
// name and is shown on a card; it is not an identity anything else keys on.
const maxNameLen = 32

// ValidName reports whether name is a legal account name: 1-32 of [a-z0-9-],
// not starting or ending with '-'.
func ValidName(name string) bool {
	if name == "" || len(name) > maxNameLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			if i == 0 || i == len(name)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// EnvSuffix is the part of an account's token variable after the prefix:
// "work-2" -> "WORK_2". It is the one place the name-to-variable mapping lives.
func EnvSuffix(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// Fingerprint is a short, one-way identifier for a token: the first 12 hex
// characters of its sha256. It is what an auth_failed mark records so the mark
// can expire when the token is replaced, without the database holding anything
// that authenticates.
func Fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:6])
}

// Mark is what the pool remembers about one account. A zero time means "never".
type Mark struct {
	Account           string
	RateLimitedAt     time.Time
	RateLimitedDetail string
	AuthFailedAt      time.Time
	AuthFailedDetail  string
	// AuthFailedToken is the [Fingerprint] of the token that failed.
	AuthFailedToken string
}

// Store persists marks and counts live agents per account — PER OWNER.
//
// 🔴 AN ACCOUNT IS (owner, name), NOT A NAME. Members bring their own accounts
// (internal/credentials), and two owners may each call one "work": they are
// different subscriptions, so a rate-limit mark on one must not push the other
// down the order, and one owner's live agents must not count against the
// other's. Every method takes the owner and every query names it (migration
// 0011 keys cc_account_marks on (owner_id, account)).
type Store interface {
	Marks(ctx context.Context, owner int64) (map[string]Mark, error)
	LiveCounts(ctx context.Context, owner int64) (map[string]int, error)
	MarkRateLimited(ctx context.Context, owner int64, account, detail string, at time.Time) error
	MarkAuthFailed(ctx context.Context, owner int64, account, fingerprint, detail string, at time.Time) error
}

// Source answers which Claude accounts an OWNER's agents may run on: name →
// setup-token. An empty map is "this owner has none", which Select refuses
// with [ErrNoUsableAccount]. internal/credentials.Plane is the production
// Source (the environment pool for the admin, the member's own tokens for a
// member); [Static] is a fixed map.
type Source interface {
	Accounts(ctx context.Context, owner int64) (map[string]string, error)
}

// Static is a fixed account map, the same for every owner: the environment
// pool. Build it with [NewStatic], which validates it.
type Static struct{ accounts map[string]string }

// NewStatic validates an account map. It REFUSES an empty pool, an invalid
// name and an empty token, so a misconfiguration stops the process at boot
// instead of building a pod with no credential at the first dispatch.
func NewStatic(accounts map[string]string) (Static, error) {
	if len(accounts) == 0 {
		return Static{}, fmt.Errorf("%w: the pool is empty", ErrNoUsableAccount)
	}
	out := make(map[string]string, len(accounts))
	for name, tok := range accounts {
		if !ValidName(name) {
			return Static{}, fmt.Errorf("ccpool: account name %q is not 1-%d of [a-z0-9-] (not starting or ending with '-')", name, maxNameLen)
		}
		if strings.TrimSpace(tok) == "" {
			return Static{}, fmt.Errorf("ccpool: account %q has an empty token", name)
		}
		out[name] = tok
	}
	return Static{accounts: out}, nil
}

// Accounts implements [Source]: a copy, whoever asks.
func (s Static) Accounts(context.Context, int64) (map[string]string, error) {
	out := make(map[string]string, len(s.accounts))
	for k, v := range s.accounts {
		out[k] = v
	}
	return out, nil
}

// The defaults of [Limits].
const (
	// DefaultCooldown is how long an account rests after a `rate_limited` whose
	// message states no reset time this package can read.
	DefaultCooldown = time.Hour
	// DefaultMaxConcurrentJobs is how many jobs one account may run at once.
	DefaultMaxConcurrentJobs = 2
)

// Limits are the pool's two knobs: the cooldown after a rate limit, and the
// per-account job concurrency cap.
type Limits struct {
	// Cooldown is the rest after a `rate_limited` whose message carries no
	// readable reset time. Positive.
	Cooldown time.Duration
	// MaxConcurrentJobs caps the jobs one account runs at once. At least 1.
	MaxConcurrentJobs int
}

// DefaultLimits is [DefaultCooldown] and [DefaultMaxConcurrentJobs].
func DefaultLimits() Limits {
	return Limits{Cooldown: DefaultCooldown, MaxConcurrentJobs: DefaultMaxConcurrentJobs}
}

// Pool is selection and marking over a per-owner [Source] and its [Store].
type Pool struct {
	src    Source
	store  Store
	now    func() time.Time
	limits Limits
}

// New builds a pool with [DefaultLimits].
func New(src Source, store Store) (*Pool, error) {
	return NewWithLimits(src, store, DefaultLimits())
}

// NewWithLimits builds a pool with explicit limits. It refuses a non-positive
// cooldown and a cap below 1: either would be a knob that silently disables
// the guard it configures.
func NewWithLimits(src Source, store Store, l Limits) (*Pool, error) {
	if src == nil {
		return nil, errors.New("ccpool: a Source is required (it says whose accounts an owner may use)")
	}
	if store == nil {
		return nil, errors.New("ccpool: a Store is required (marks and live counts are what selection reads)")
	}
	if l.Cooldown <= 0 {
		return nil, fmt.Errorf("ccpool: the rate-limit cooldown must be positive, got %s", l.Cooldown)
	}
	if l.MaxConcurrentJobs < 1 {
		return nil, fmt.Errorf("ccpool: the per-account job cap must be at least 1, got %d", l.MaxConcurrentJobs)
	}
	return &Pool{src: src, store: store, now: time.Now, limits: l}, nil
}

func (p *Pool) accounts(ctx context.Context, owner int64) (map[string]string, []string, error) {
	acc, err := p.src.Accounts(ctx, owner)
	if err != nil {
		return nil, nil, fmt.Errorf("ccpool: read the Claude accounts of user %d: %w", owner, err)
	}
	names := make([]string, 0, len(acc))
	for n := range acc {
		names = append(names, n)
	}
	sort.Strings(names)
	return acc, names, nil
}

// Names returns owner's account names, sorted. Never the tokens.
func (p *Pool) Names(ctx context.Context, owner int64) ([]string, error) {
	_, names, err := p.accounts(ctx, owner)
	return names, err
}

// Token returns the setup-token of owner's account. It is how a pod spec gets
// its token; [ErrUnknownAccount] when owner has no such account — including
// when ANOTHER owner does.
func (p *Pool) Token(ctx context.Context, owner int64, account string) (string, error) {
	acc, _, err := p.accounts(ctx, owner)
	if err != nil {
		return "", err
	}
	tok, ok := acc[account]
	if !ok || tok == "" {
		return "", fmt.Errorf("%w %q for user %d", ErrUnknownAccount, account, owner)
	}
	return tok, nil
}

// Select picks the account for owner's new claude-code agent, from OWNER'S
// accounts only. A non-empty pin is returned as-is when it names one of them,
// and refused with [ErrUnknownAccount] otherwise.
func (p *Pool) Select(ctx context.Context, owner int64, pin string) (string, error) {
	tokens, names, err := p.accounts(ctx, owner)
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", fmt.Errorf("%w: you have no Claude account", ErrNoUsableAccount)
	}
	if pin != "" {
		if _, ok := tokens[pin]; !ok {
			return "", fmt.Errorf("%w %q (yours: %s)", ErrUnknownAccount, pin, strings.Join(names, ", "))
		}
		return pin, nil
	}
	marks, err := p.store.Marks(ctx, owner)
	if err != nil {
		return "", fmt.Errorf("ccpool: read account marks: %w", err)
	}
	live, err := p.store.LiveCounts(ctx, owner)
	if err != nil {
		return "", fmt.Errorf("ccpool: count live agents per account: %w", err)
	}
	return Choose(p.now(), p.limits.Cooldown, names, tokens, marks, live)
}

// AuthFailure reports why owner's account cannot authenticate: a non-empty
// sentence when its CURRENT token is marked auth-failed (the mark's fingerprint
// matches it, exactly as [Choose] skips it), "" when it is usable. An unknown
// account is [ErrUnknownAccount].
//
// ⚠ Select's PIN PATH DELIBERATELY DOES NOT CALL THIS: a pin is the operator's
// override (TestSelectHonoursAPinOverEverySelectionRule). A caller that must not
// spend an irreversible step on a dead account — the chief replacement, which
// deletes the old chief first — asks here itself.
func (p *Pool) AuthFailure(ctx context.Context, owner int64, account string) (string, error) {
	tokens, _, err := p.accounts(ctx, owner)
	if err != nil {
		return "", err
	}
	tok, ok := tokens[account]
	if !ok || tok == "" {
		return "", fmt.Errorf("%w %q for user %d", ErrUnknownAccount, account, owner)
	}
	marks, err := p.store.Marks(ctx, owner)
	if err != nil {
		return "", fmt.Errorf("ccpool: read account marks: %w", err)
	}
	m := marks[account]
	if !authFailedNow(m, tok) {
		return "", nil
	}
	return fmt.Sprintf("Claude account %q failed authentication at %s (%s); replace its token first",
		account, m.AuthFailedAt.UTC().Format(time.RFC3339), m.AuthFailedDetail), nil
}

// AccountState is what selection knows about one of an owner's accounts.
type AccountState struct {
	// AuthFailed: the account's CURRENT token is marked auth_failed (the
	// mark's fingerprint is that token's), exactly the case [Choose] skips.
	AuthFailed bool
	// RateLimitedAt is the account's latest rate_limited mark; zero for never.
	RateLimitedAt time.Time
	// CoolingUntil is when the account's rate-limit cooldown ends, set ONLY
	// while it is still cooling down at the time States read it; zero otherwise.
	CoolingUntil time.Time
	// MaxConcurrentJobs is the per-account job cap ([Limits]).
	MaxConcurrentJobs int
}

// States is AccountState for every one of owner's configured accounts, by
// name. An account that is not configured for owner is absent. It is what the
// job API's worker-pool selection reads (internal/api createPoolJob): the same
// marks, under the same "current token" rule, as [Choose].
func (p *Pool) States(ctx context.Context, owner int64) (map[string]AccountState, error) {
	tokens, names, err := p.accounts(ctx, owner)
	if err != nil {
		return nil, err
	}
	marks, err := p.store.Marks(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("ccpool: read account marks: %w", err)
	}
	out := make(map[string]AccountState, len(names))
	for _, n := range names {
		m := marks[n]
		out[n] = AccountState{AuthFailed: authFailedNow(m, tokens[n]), RateLimitedAt: m.RateLimitedAt}
	}
	return out, nil
}

// authFailedNow reports whether m marks token — the account's current one —
// as having failed authentication.
func authFailedNow(m Mark, token string) bool {
	return !m.AuthFailedAt.IsZero() && m.AuthFailedToken != "" && m.AuthFailedToken == Fingerprint(token)
}

// CooldownUntil is when m's rate-limit cooldown ends; zero for an account
// never rate-limited.
func CooldownUntil(m Mark, cooldown time.Duration) time.Time {
	return time.Time{} // SCAFFOLDING: implemented in the next commit.
}

// Choose is the selection algorithm (see the package doc), pure.
func Choose(now time.Time, cooldown time.Duration, names []string, tokens map[string]string, marks map[string]Mark, live map[string]int) (string, error) {
	type cand struct {
		name string
		rl   time.Time
		live int
	}
	var cands []cand
	var skipped []string
	for _, n := range names {
		m := marks[n]
		if authFailedNow(m, tokens[n]) {
			skipped = append(skipped, fmt.Sprintf("%s (auth failed %s: %s)", n, m.AuthFailedAt.UTC().Format(time.RFC3339), m.AuthFailedDetail))
			continue
		}
		cands = append(cands, cand{name: n, rl: m.RateLimitedAt, live: live[n]})
	}
	if len(cands) == 0 {
		if len(skipped) == 0 {
			return "", fmt.Errorf("%w: the pool is empty", ErrNoUsableAccount)
		}
		return "", fmt.Errorf("%w: every account's current token failed authentication — %s. Replace the "+
			"token (claude setup-token) to clear the mark, or pin an account explicitly",
			ErrNoUsableAccount, strings.Join(skipped, "; "))
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if !a.rl.Equal(b.rl) {
			if a.rl.IsZero() || b.rl.IsZero() {
				return a.rl.IsZero()
			}
			return a.rl.Before(b.rl)
		}
		if a.live != b.live {
			return a.live < b.live
		}
		return a.name < b.name
	})
	return cands[0].name, nil
}

// maxDetail bounds the stored detail. ccd's message carries the CLI's "resets …"
// text, which is what is worth keeping; a runtime body is not.
const maxDetail = 300

// MarkFailure records a typed ccd failure against owner's account. Types other
// than [FailureRateLimited] and [FailureAuthFailed] are ignored (nil): only
// those two say something about the ACCOUNT rather than about the turn.
func (p *Pool) MarkFailure(ctx context.Context, owner int64, account, failure, detail string) error {
	if failure != FailureRateLimited && failure != FailureAuthFailed {
		return nil
	}
	tok, err := p.Token(ctx, owner, account)
	if err != nil {
		return err
	}
	if len(detail) > maxDetail {
		// Cut on a rune boundary: a byte cut through "·" or "…" is invalid UTF-8,
		// which Postgres refuses for TEXT (22021) — losing the mark.
		cut := maxDetail
		for cut > 0 && !utf8.RuneStart(detail[cut]) {
			cut--
		}
		detail = detail[:cut] + "…"
	}
	if failure == FailureRateLimited {
		return p.store.MarkRateLimited(ctx, owner, account, detail, p.now())
	}
	return p.store.MarkAuthFailed(ctx, owner, account, Fingerprint(tok), detail, p.now())
}
