package attention

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// Users in this package's scratch database: 1 is the seeded operator; 2 and 3
// are members.
func pgStore(t *testing.T) (*PGStore, *pgxpool.Pool, string) {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(dbtest.AsOperator(ctx), pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'member') ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `TRUNCATE attention_asks`)
	return NewPG(pool), pool, dsn
}

func as(uid int64) context.Context {
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
}

var session = Asker{MirrorHost: "laptop", MirrorSession: "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b", Label: "work:1 @ laptop"}

func mustAsk(t *testing.T, s *PGStore, uid int64, n New) Ask {
	t.Helper()
	a, err := s.Create(as(uid), n)
	if err != nil {
		t.Fatalf("ask as %d: %v", uid, err)
	}
	return a
}

// TestAnAskIsAnsweredOnceAndReadsBackWithItsAnswer: create → open → answer;
// the answered ask carries the answer and who gave it, and a second answer or a
// cancel is refused with the row unchanged.
func TestAnAskIsAnsweredOnceAndReadsBackWithItsAnswer(t *testing.T) {
	s, _, _ := pgStore(t)
	a := mustAsk(t, s, 2, New{Asker: session, Question: "  Deploy now or wait?  ", Options: []string{"now", " wait ", ""}})
	if a.Status != Open || a.Question != "Deploy now or wait?" || strings.Join(a.Options, "|") != "now|wait" || a.Owner != 2 {
		t.Fatalf("created %+v", a)
	}
	if d := a.ExpiresAt.Sub(a.CreatedAt); d < DefaultTimeout-time.Second || d > DefaultTimeout+time.Second {
		t.Fatalf("default expiry %s, want %s", d, DefaultTimeout)
	}
	open, err := s.Open(as(2))
	if err != nil || len(open) != 1 || open[0].ID != a.ID {
		t.Fatalf("open: %v %+v", err, open)
	}
	got, err := s.Answer(as(2), a.ID, "wait", ByOperator)
	if err != nil || got.Status != Answered || got.Answer != "wait" || got.AnsweredBy != ByOperator || got.ResolvedAt.IsZero() {
		t.Fatalf("answer: %v %+v", err, got)
	}
	if again, err := s.Answer(as(2), a.ID, "now", ByChief); !errors.Is(err, ErrNotOpen) || again.Answer != "wait" {
		t.Fatalf("second answer: %v %+v", err, again)
	}
	if _, err := s.Cancel(as(2), a.ID); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("cancel after answer: %v", err)
	}
	if open, _ := s.Open(as(2)); len(open) != 0 {
		t.Fatalf("an answered ask is still open: %+v", open)
	}
}

// TestRowLevelSecurityKeepsAsksToTheirOwner: another member cannot read,
// list, answer or cancel an ask — each reads as not found.
func TestRowLevelSecurityKeepsAsksToTheirOwner(t *testing.T) {
	s, _, _ := pgStore(t)
	a := mustAsk(t, s, 2, New{Asker: session, Question: "which branch?"})
	if _, err := s.Get(as(3), a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get as 3: %v", err)
	}
	if open, err := s.Open(as(3)); err != nil || len(open) != 0 {
		t.Fatalf("open as 3: %v %+v", err, open)
	}
	if _, err := s.Answer(as(3), a.ID, "main", ByOperator); !errors.Is(err, ErrNotFound) {
		t.Fatalf("answer as 3: %v", err)
	}
	if _, err := s.Cancel(as(3), a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel as 3: %v", err)
	}
	if got, err := s.Get(as(2), a.ID); err != nil || got.Status != Open {
		t.Fatalf("owner's ask moved: %v %+v", err, got)
	}
}

// TestAnAgentAskNamesOnlyItsOwnersAgent: an ask naming an agent of another
// owner is refused by 0025's owner trigger.
func TestAnAgentAskNamesOnlyItsOwnersAgent(t *testing.T) {
	s, pool, _ := pgStore(t)
	var agent int64
	if err := pool.QueryRow(as(3), `INSERT INTO agents (name, namespace) VALUES ($1, $1) RETURNING id`,
		"attn-agent-"+time.Now().Format("150405.000000")).Scan(&agent); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, agent)
	})
	if _, err := s.Create(as(2), New{Asker: Asker{AgentID: agent}, Question: "mine?"}); err == nil {
		t.Fatal("an ask named another owner's agent")
	}
	a := mustAsk(t, s, 3, New{Asker: Asker{AgentID: agent, Label: "worker"}, Question: "mine?"})
	if a.Asker.AgentID != agent || a.Asker.MirrorHost != "" {
		t.Fatalf("%+v", a)
	}
}

// TestCancelAndExpiry: a cancelled ask is not open and cannot be answered; an
// ask past its expiry reads as expired before the sweep, cannot be answered,
// and ExpireDue moves it (once) and hands it back with its owner.
func TestCancelAndExpiry(t *testing.T) {
	s, pool, _ := pgStore(t)
	c := mustAsk(t, s, 2, New{Asker: session, Question: "cancel me"})
	if got, err := s.Cancel(as(2), c.ID); err != nil || got.Status != Cancelled || got.ResolvedAt.IsZero() {
		t.Fatalf("cancel: %v %+v", err, got)
	}
	if _, err := s.Answer(as(2), c.ID, "too late", ByOperator); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("answer after cancel: %v", err)
	}
	e := mustAsk(t, s, 2, New{Asker: session, Question: "expire me", Timeout: MinTimeout})
	if d := e.ExpiresAt.Sub(e.CreatedAt); d < MinTimeout-time.Second || d > MinTimeout+time.Second {
		t.Fatalf("expiry %s", d)
	}
	// Age it past its expiry on the database's clock.
	if _, err := pool.Exec(authz.System(context.Background()), `UPDATE attention_asks SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second' WHERE id = $1`, e.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(as(2), e.ID); err != nil || got.Status != Expired {
		t.Fatalf("lapsed ask reads %v %+v", err, got)
	}
	if open, _ := s.Open(as(2)); len(open) != 0 {
		t.Fatalf("a lapsed ask is listed open: %+v", open)
	}
	if _, err := s.Answer(as(2), e.ID, "late", ByOperator); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("answer after expiry: %v", err)
	}
	due, err := s.ExpireDue(context.Background())
	if err != nil || len(due) != 1 || due[0].ID != e.ID || due[0].Owner != 2 || due[0].Status != Expired {
		t.Fatalf("expire: %v %+v", err, due)
	}
	if due, _ := s.ExpireDue(context.Background()); len(due) != 0 {
		t.Fatalf("expired twice: %+v", due)
	}
}

// TestAnswerAndCancelRaceToOneOutcome: many concurrent answers and cancels on
// one ask — exactly one wins, every loser reads ErrNotOpen.
func TestAnswerAndCancelRaceToOneOutcome(t *testing.T) {
	s, _, _ := pgStore(t)
	a := mustAsk(t, s, 2, New{Asker: session, Question: "race"})
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, lost := 0, 0
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				_, err = s.Answer(as(2), a.ID, "yes", ByOperator)
			} else {
				_, err = s.Cancel(as(2), a.ID)
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrNotOpen):
				lost++
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if won != 1 || lost != 11 {
		t.Fatalf("won %d lost %d, want 1/11", won, lost)
	}
}

// TestOpenAsksAreCappedPerOwner: the MaxOpenPerOwner+1th open ask is refused;
// another owner is unaffected.
func TestOpenAsksAreCappedPerOwner(t *testing.T) {
	s, _, _ := pgStore(t)
	for range MaxOpenPerOwner {
		mustAsk(t, s, 2, New{Asker: session, Question: "q"})
	}
	if _, err := s.Create(as(2), New{Asker: session, Question: "one too many"}); !errors.Is(err, ErrTooMany) {
		t.Fatalf("past the cap: %v", err)
	}
	mustAsk(t, s, 3, New{Asker: session, Question: "another owner"})
}

func TestCleanBounds(t *testing.T) {
	long := strings.Repeat("x", MaxQuestionRunes+1)
	for name, n := range map[string]New{
		"no asker":     {Question: "q"},
		"two askers":   {Asker: Asker{AgentID: 1, MirrorHost: "h", MirrorSession: "s"}, Question: "q"},
		"bad session":  {Asker: Asker{MirrorHost: "h", MirrorSession: "../x"}, Question: "q"},
		"empty":        {Asker: session, Question: "  "},
		"long":         {Asker: session, Question: long},
		"dup option":   {Asker: session, Question: "q", Options: []string{"a", " a"}},
		"long option":  {Asker: session, Question: "q", Options: []string{strings.Repeat("o", MaxOptionRunes+1)}},
		"many options": {Asker: session, Question: "q", Options: strings.Split("a b c d e f g h i j k", " ")},
	} {
		if _, err := Clean(n); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n, _ := Clean(New{Asker: session, Question: "q", Timeout: time.Second}); n.Timeout != MinTimeout {
		t.Errorf("short timeout -> %s", n.Timeout)
	}
	if n, _ := Clean(New{Asker: session, Question: "q", Timeout: 30 * 24 * time.Hour}); n.Timeout != MaxTimeout {
		t.Errorf("long timeout -> %s", n.Timeout)
	}
	if _, err := CleanAnswer(strings.Repeat("a", MaxAnswerRunes+1)); !errors.Is(err, ErrInvalid) {
		t.Errorf("long answer: %v", err)
	}
}
