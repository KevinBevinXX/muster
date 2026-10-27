package ccpool

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Fake tokens: obviously not real, and pairwise distinct so a mix-up between two
// accounts' tokens is visible.
const (
	tokAlpha = "sk-ant-oat01-FAKE-alpha-not-a-real-token"
	tokBeta  = "sk-ant-oat01-FAKE-beta-not-a-real-token"
	tokGamma = "sk-ant-oat01-FAKE-gamma-not-a-real-token"
)

var (
	names  = []string{"alpha", "beta", "gamma"}
	tokens = map[string]string{"alpha": tokAlpha, "beta": tokBeta, "gamma": tokGamma}
	t0     = time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)
	// rested is a "now" long after every mark the ranking tests write, so no
	// account in them is still cooling down and the ranking alone decides.
	rested = t0.Add(30 * 24 * time.Hour)
)

func choose(t *testing.T, marks map[string]Mark, live map[string]int) string {
	t.Helper()
	got, err := Choose(rested, DefaultCooldown, names, tokens, marks, live)
	if err != nil {
		t.Fatalf("Choose: %v", err)
	}
	return got
}

func TestChooseNeverRateLimitedBeatsAnyRateLimitedAccount(t *testing.T) {
	// alpha would win on name and on live count; only its rate limit can lose it.
	marks := map[string]Mark{"alpha": {RateLimitedAt: t0}}
	live := map[string]int{"beta": 3, "gamma": 5}
	if got := choose(t, marks, live); got != "beta" {
		t.Fatalf("got %q, want beta: a never-rate-limited account outranks a rate-limited one, "+
			"even one with fewer live agents", got)
	}
}

func TestChooseLeastRecentlyRateLimitedFirst(t *testing.T) {
	marks := map[string]Mark{
		"alpha": {RateLimitedAt: t0.Add(2 * time.Hour)},
		"beta":  {RateLimitedAt: t0.Add(1 * time.Hour)},
		"gamma": {RateLimitedAt: t0.Add(3 * time.Hour)},
	}
	if got := choose(t, marks, nil); got != "beta" {
		t.Fatalf("got %q, want beta (rate-limited longest ago)", got)
	}
}

func TestChooseFewestLiveAgentsBreaksARateLimitTie(t *testing.T) {
	// No account rate-limited: the live count decides, and alpha (first by name)
	// must LOSE to the one with fewer live agents.
	live := map[string]int{"alpha": 2, "beta": 1, "gamma": 4}
	if got := choose(t, nil, live); got != "beta" {
		t.Fatalf("got %q, want beta (fewest live agents)", got)
	}
	// Same rate-limit timestamp on all three: still the live count.
	marks := map[string]Mark{"alpha": {RateLimitedAt: t0}, "beta": {RateLimitedAt: t0}, "gamma": {RateLimitedAt: t0}}
	live = map[string]int{"alpha": 5, "beta": 7, "gamma": 1}
	if got := choose(t, marks, live); got != "gamma" {
		t.Fatalf("got %q, want gamma (equal rate limits, fewest live)", got)
	}
}

func TestChooseNameIsTheFinalTiebreak(t *testing.T) {
	if got := choose(t, nil, nil); got != "alpha" {
		t.Fatalf("got %q, want alpha", got)
	}
}

func TestChooseSkipsAnAccountWhoseCurrentTokenFailedAuth(t *testing.T) {
	marks := map[string]Mark{"alpha": {AuthFailedAt: t0, AuthFailedToken: Fingerprint(tokAlpha), AuthFailedDetail: "401"}}
	if got := choose(t, marks, nil); got != "beta" {
		t.Fatalf("got %q, want beta: alpha's CURRENT token failed authentication", got)
	}
}

func TestAnAuthFailureOnAReplacedTokenNoLongerSkipsTheAccount(t *testing.T) {
	// The mark's fingerprint is of a token the operator has since replaced.
	marks := map[string]Mark{"alpha": {AuthFailedAt: t0, AuthFailedToken: Fingerprint("sk-ant-oat01-FAKE-old-token")}}
	if got := choose(t, marks, nil); got != "alpha" {
		t.Fatalf("got %q, want alpha: an auth failure recorded against a REPLACED token must not exclude the account", got)
	}
}

func TestChooseRefusesWhenEveryAccountsTokenFailedAuth(t *testing.T) {
	marks := map[string]Mark{}
	for n, tok := range tokens {
		marks[n] = Mark{AuthFailedAt: t0, AuthFailedToken: Fingerprint(tok), AuthFailedDetail: "OAuth token invalid"}
	}
	_, err := Choose(rested, DefaultCooldown, names, tokens, marks, nil)
	if !errors.Is(err, ErrNoUsableAccount) {
		t.Fatalf("err = %v, want ErrNoUsableAccount", err)
	}
	for _, tok := range tokens {
		if strings.Contains(err.Error(), tok) {
			t.Fatalf("the refusal carries a token: %v", err)
		}
	}
	if !strings.Contains(err.Error(), "alpha") {
		t.Fatalf("the refusal does not name the accounts: %v", err)
	}
}

// fakeStore records writes and serves canned marks/counts. It records the
// OWNER every read and write was asked for, so a pool that consulted another
// owner's marks (or wrote a mark against the wrong owner) shows.
type fakeStore struct {
	marks map[string]Mark
	live  map[string]int
	calls []string
	asked []int64
	err   error
}

func (f *fakeStore) Marks(_ context.Context, owner int64) (map[string]Mark, error) {
	f.asked = append(f.asked, owner)
	return f.marks, f.err
}
func (f *fakeStore) LiveCounts(_ context.Context, owner int64) (map[string]int, error) {
	f.asked = append(f.asked, owner)
	return f.live, f.err
}
func (f *fakeStore) MarkRateLimited(_ context.Context, owner int64, a, d string, _ time.Time) error {
	f.calls = append(f.calls, "rate_limited|"+strconv.FormatInt(owner, 10)+"|"+a+"|"+d)
	return f.err
}
func (f *fakeStore) MarkAuthFailed(_ context.Context, owner int64, a, fp, d string, _ time.Time) error {
	f.calls = append(f.calls, "auth_failed|"+strconv.FormatInt(owner, 10)+"|"+a+"|"+fp+"|"+d)
	return f.err
}

// owned is a per-owner Source: each owner's own account map, nothing for an
// owner it does not know.
type owned map[int64]map[string]string

func (o owned) Accounts(_ context.Context, owner int64) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range o[owner] {
		out[k] = v
	}
	return out, nil
}

// opOwner is the owner the single-pool tests below select for.
const opOwner int64 = 1

func newPool(t *testing.T, st Store) *Pool {
	t.Helper()
	src, err := NewStatic(tokens)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(src, st)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestStatesReportsWhatChooseWouldSkip: per configured account, AuthFailed is
// true exactly for a mark against the CURRENT token (alpha), not for one
// against a replaced token (beta), and RateLimitedAt is the mark's; an account
// with no mark (gamma) is present and clean; a marked name that is not
// configured is absent. Read for the owner asked.
func TestStatesReportsWhatChooseWouldSkip(t *testing.T) {
	st := &fakeStore{marks: map[string]Mark{
		"alpha":   {AuthFailedAt: t0, AuthFailedToken: Fingerprint(tokAlpha), RateLimitedAt: t0.Add(time.Hour)},
		"beta":    {AuthFailedAt: t0, AuthFailedToken: Fingerprint("sk-ant-oat01-FAKE-old-token"), RateLimitedAt: t0},
		"retired": {AuthFailedAt: t0, AuthFailedToken: "x"},
	}}
	got, err := newPool(t, st).States(context.Background(), opOwner)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]AccountState{
		"alpha": {AuthFailed: true, RateLimitedAt: t0.Add(time.Hour)},
		"beta":  {RateLimitedAt: t0},
		"gamma": {},
	}
	if len(got) != len(want) {
		t.Fatalf("States = %v, want %v", got, want)
	}
	for n, w := range want {
		if g, ok := got[n]; !ok || g.AuthFailed != w.AuthFailed || !g.RateLimitedAt.Equal(w.RateLimitedAt) {
			t.Errorf("States[%s] = %+v (present %v), want %+v", n, g, ok, w)
		}
	}
	if len(st.asked) != 1 || st.asked[0] != opOwner {
		t.Fatalf("marks read for owners %v, want [%d]", st.asked, opOwner)
	}
}

func TestSelectHonoursAPinOverEverySelectionRule(t *testing.T) {
	// gamma loses on every automatic rule — rate-limited most recently, most live
	// agents, last by name, and its current token failed auth — and a pin still
	// gets it, because the pin is the operator's override.
	st := &fakeStore{
		marks: map[string]Mark{"gamma": {RateLimitedAt: t0, AuthFailedAt: t0, AuthFailedToken: Fingerprint(tokGamma)}},
		live:  map[string]int{"gamma": 9},
	}
	p := newPool(t, st)
	got, err := p.Select(context.Background(), opOwner, "gamma")
	if err != nil || got != "gamma" {
		t.Fatalf("Select(pin gamma) = %q, %v; want gamma", got, err)
	}
	auto, err := p.Select(context.Background(), opOwner, "")
	if err != nil || auto != "alpha" {
		t.Fatalf("Select(auto) = %q, %v; want alpha (the control: gamma is NOT what auto picks)", auto, err)
	}
	if _, err := p.Select(context.Background(), opOwner, "delta"); !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("Select(pin delta) err = %v, want ErrUnknownAccount", err)
	}
}

func TestSelectReadsTheStoresMarksAndLiveCounts(t *testing.T) {
	st := &fakeStore{marks: map[string]Mark{"alpha": {RateLimitedAt: t0}}, live: map[string]int{"beta": 1}}
	p := newPool(t, st)
	got, err := p.Select(context.Background(), opOwner, "")
	if err != nil || got != "gamma" {
		t.Fatalf("Select = %q, %v; want gamma (alpha rate-limited, beta has a live agent)", got, err)
	}
}

func TestMarkFailureRecordsRateLimitAndAuthFailureNeverTheToken(t *testing.T) {
	st := &fakeStore{}
	p := newPool(t, st)
	ctx := context.Background()
	if err := p.MarkFailure(ctx, opOwner, "beta", FailureRateLimited, "You've hit your limit · resets 5pm"); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkFailure(ctx, opOwner, "beta", FailureAuthFailed, "OAuth token invalid"); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkFailure(ctx, opOwner, "beta", "turn_timeout", "ignored"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"rate_limited|1|beta|You've hit your limit · resets 5pm",
		"auth_failed|1|beta|" + Fingerprint(tokBeta) + "|OAuth token invalid",
	}
	if strings.Join(st.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("store calls:\n%s\nwant:\n%s", strings.Join(st.calls, "\n"), strings.Join(want, "\n"))
	}
	for _, c := range st.calls {
		if strings.Contains(c, tokBeta) {
			t.Fatalf("a stored mark carries the token: %q", c)
		}
	}
	if err := p.MarkFailure(ctx, opOwner, "nope", FailureRateLimited, ""); !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("unknown account err = %v", err)
	}
}

func TestFingerprintIsPinnedAndShort(t *testing.T) {
	// sha256("sk-ant-oat01-FAKE-alpha-not-a-real-token")[:6] in hex.
	got := Fingerprint(tokAlpha)
	if len(got) != 12 || got == Fingerprint(tokBeta) {
		t.Fatalf("Fingerprint = %q", got)
	}
	if strings.Contains(tokAlpha, got) {
		t.Fatalf("fingerprint %q is a substring of the token", got)
	}
}

func TestNewStaticRefusesAnEmptyPoolABadNameAndAnEmptyToken(t *testing.T) {
	cases := map[string]map[string]string{
		"empty":       {},
		"bad name":    {"Alpha": tokAlpha},
		"dash edge":   {"-alpha": tokAlpha},
		"empty token": {"alpha": "  "},
	}
	for name, acc := range cases {
		if _, err := NewStatic(acc); err == nil {
			t.Errorf("%s: NewStatic accepted %v", name, acc)
		} else if strings.Contains(err.Error(), tokAlpha) {
			t.Errorf("%s: refusal carries the token: %v", name, err)
		}
	}
	src, _ := NewStatic(tokens)
	if _, err := New(src, nil); err == nil {
		t.Error("New accepted a nil store")
	}
	if _, err := New(nil, &fakeStore{}); err == nil {
		t.Error("New accepted a nil source")
	}
}

func TestEnvSuffix(t *testing.T) {
	for in, want := range map[string]string{"work": "WORK", "work-2": "WORK_2", "a-b-c": "A_B_C"} {
		if got := EnvSuffix(in); got != want {
			t.Errorf("EnvSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestALongDetailIsCutOnARuneBoundary: a byte cut through a multibyte rune is
// invalid UTF-8, which Postgres refuses for TEXT — losing the mark.
func TestALongDetailIsCutOnARuneBoundary(t *testing.T) {
	st := &fakeStore{}
	p := newPool(t, st)
	detail := strings.Repeat("a", maxDetail-1) + "·resets 5pm" // "·" is 2 bytes and straddles the cut
	if err := p.MarkFailure(context.Background(), opOwner, "alpha", FailureRateLimited, detail); err != nil {
		t.Fatal(err)
	}
	stored := strings.TrimPrefix(st.calls[0], "rate_limited|1|alpha|")
	if !utf8.ValidString(stored) || !strings.HasSuffix(stored, "…") || len(stored) > maxDetail+len("…") {
		t.Fatalf("stored detail is invalid or over-long: %q", stored)
	}
}

// TestSelectChoosesOnlyAmongTheOwnersOwnAccounts: two owners, each with an
// account the OTHER's automatic rule would prefer. Each owner's selection, pin
// and token lookup stay inside that owner's own accounts, and the store is
// asked about exactly that owner.
func TestSelectChoosesOnlyAmongTheOwnersOwnAccounts(t *testing.T) {
	src := owned{
		1: {"alpha": tokAlpha}, // the admin's ("alpha" wins every tiebreak)
		7: {"zulu": "sk-ant-oat01-FAKE-zulu-member-token"},
	}
	st := &fakeStore{}
	p, err := New(src, st)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		got, err := p.Select(ctx, 7, "")
		if err != nil || got != "zulu" {
			t.Fatalf("member Select = %q, %v; want zulu, the member's ONLY account", got, err)
		}
	}
	for _, o := range st.asked {
		if o != 7 {
			t.Fatalf("the member's selection read the store for owner %d", o)
		}
	}
	if _, err := p.Select(ctx, 7, "alpha"); !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("a member pinning the admin's account: err = %v, want ErrUnknownAccount", err)
	}
	if _, err := p.Token(ctx, 7, "alpha"); !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("a member's agent on account alpha got a token (err %v)", err)
	}
	if got, err := p.Select(ctx, 1, ""); err != nil || got != "alpha" {
		t.Fatalf("control: the admin's Select = %q, %v; want alpha", got, err)
	}
	if _, err := p.Select(ctx, 9, ""); !errors.Is(err, ErrNoUsableAccount) {
		t.Fatalf("an owner with no account: err = %v, want ErrNoUsableAccount", err)
	}
	if err := p.MarkFailure(ctx, 7, "zulu", FailureRateLimited, "x"); err != nil {
		t.Fatal(err)
	}
	if got := st.calls[len(st.calls)-1]; got != "rate_limited|7|zulu|x" {
		t.Fatalf("mark = %q, want it against owner 7", got)
	}
	if err := p.MarkFailure(ctx, 7, "alpha", FailureRateLimited, "x"); !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("a member's agent marking the admin's account: err = %v", err)
	}
}

// TestAuthFailureNamesADeadAccountAndOnlyForItsCurrentToken: the agent
// replacement's pre-flight. A mark against the CURRENT token's fingerprint is
// a reason; a mark against a rotated-away token, a rate limit, or no mark is
// not; an account the owner lacks is ErrUnknownAccount.
func TestAuthFailureNamesADeadAccountAndOnlyForItsCurrentToken(t *testing.T) {
	st := &fakeStore{marks: map[string]Mark{
		"gamma": {AuthFailedAt: t0, AuthFailedDetail: "invalid x-api-key", AuthFailedToken: Fingerprint(tokGamma)},
		"beta":  {AuthFailedAt: t0, AuthFailedToken: Fingerprint("a-token-since-rotated-away")},
		"alpha": {RateLimitedAt: t0},
	}}
	p := newPool(t, st)
	why, err := p.AuthFailure(context.Background(), opOwner, "gamma")
	if err != nil || !strings.Contains(why, `"gamma" failed authentication`) || !strings.Contains(why, "invalid x-api-key") {
		t.Fatalf("gamma: %q %v", why, err)
	}
	for _, n := range []string{"beta", "alpha"} {
		if why, err := p.AuthFailure(context.Background(), opOwner, n); err != nil || why != "" {
			t.Fatalf("%s: %q %v; want usable", n, why, err)
		}
	}
	if _, err := p.AuthFailure(context.Background(), opOwner, "delta"); !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("delta: %v", err)
	}
}
