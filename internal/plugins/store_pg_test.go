package plugins

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// Users in this package's scratch database: 1 is the seeded operator; 2 and 3
// are members; 4 is a disabled member.
func pgStore(t *testing.T) (*PGStore, *pgxpool.Pool, string) {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'member'), (4, 'member')
		ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `UPDATE users SET disabled_at = NULL WHERE id IN (2, 3)`)
	dbtest.OwnerExec(t, dsn, `UPDATE users SET disabled_at = now() WHERE id = 4`)
	dbtest.OwnerExec(t, dsn, `TRUNCATE plugins, plugin_messages, plugin_reply_drafts, session_routes, attention_asks`)
	dbtest.OwnerExec(t, dsn, `DELETE FROM agents WHERE name LIKE 'plg-%'`)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'plugin.%'`)
	return NewPG(pool), pool, dsn
}

// as is a request principal for user uid through door via.
func as(uid int64, via authz.Via) context.Context {
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: via})
}

func session(uid int64) context.Context { return as(uid, authz.ViaSession) }
func plugin(uid int64) context.Context  { return as(uid, authz.ViaPlugin) }

func mustCreate(t *testing.T, s *PGStore, uid int64, name string) (string, Plugin) {
	t.Helper()
	tok, p, err := s.Create(session(uid), name, "Example inbox", "example-inbox")
	if err != nil {
		t.Fatalf("create %s for %d: %v", name, uid, err)
	}
	return tok, p
}

func msg(id string) MessageIn {
	return MessageIn{ExternalID: id, Sender: "someone@example.invalid", Subject: "hello", Body: "body of " + id}
}

func TestATokenIsShownOnceAndOnlyItsHashIsStored(t *testing.T) {
	s, pool, _ := pgStore(t)
	tok, p := mustCreate(t, s, 2, "example-inbox")
	if !TokenShapeOK(tok) || !strings.HasPrefix(tok, "mpl_") {
		t.Fatalf("minted token %q is not mpl_-shaped", tok)
	}
	if p.OwnerID != 2 || p.Name != "example-inbox" || p.DataScope != "example-inbox" {
		t.Fatalf("plugin = %+v", p)
	}
	var row string
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT row_to_json(t)::text FROM plugins t WHERE id = $1`, p.ID).Scan(&row); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row, strings.TrimPrefix(tok, TokenPrefix)) || !strings.Contains(row, HashToken(tok)) {
		t.Fatalf("the stored row carries the token or lacks its hash: %s", row)
	}
	got, err := s.Authenticate(authz.System(context.Background()), tok)
	if err != nil || got.ID != p.ID || got.OwnerID != 2 || got.LastSeenAt == nil {
		t.Fatalf("Authenticate = %+v, %v", got, err)
	}
	var audits int
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM audit_log
		WHERE action = 'plugin.created' AND object = $1 AND user_id = 2`, Object(p.ID)).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("plugin.created audit rows = %d (%v); want 1", audits, err)
	}
}

func TestAuthenticateRefusesEveryOtherShape(t *testing.T) {
	s, _, dsn := pgStore(t)
	tok, p := mustCreate(t, s, 2, "example-inbox")
	sys := authz.System(context.Background())
	for name, bad := range map[string]string{
		"empty":                 "",
		"runner-token shaped":   "mfr_" + strings.TrimPrefix(tok, TokenPrefix),
		"personal-token shaped": "mst_" + strings.TrimPrefix(tok, TokenPrefix),
		"unknown mpl_":          TokenPrefix + strings.Repeat("A", 43),
		"truncated":             tok[:len(tok)-1],
	} {
		if _, err := s.Authenticate(sys, bad); !errors.Is(err, ErrBadToken) {
			t.Errorf("%s: Authenticate = %v; want ErrBadToken", name, err)
		}
	}
	// A disabled owner's token stops working.
	tok4, _ := mustCreate(t, s, 3, "example-inbox")
	dbtest.OwnerExec(t, dsn, `UPDATE users SET disabled_at = now() WHERE id = 3`)
	if _, err := s.Authenticate(sys, tok4); !errors.Is(err, ErrBadToken) {
		t.Fatalf("a disabled owner's token: %v; want ErrBadToken", err)
	}
	// A revoked token stops working, and revoking is the owner's alone.
	if found, err := s.Revoke(session(3), p.ID); err != nil || found {
		t.Fatalf("user 3 revoking user 2's plugin: found=%v err=%v; want not found", found, err)
	}
	if found, err := s.Revoke(session(2), p.ID); err != nil || !found {
		t.Fatalf("revoke: found=%v err=%v", found, err)
	}
	if _, err := s.Authenticate(sys, tok); !errors.Is(err, ErrBadToken) {
		t.Fatalf("a revoked token: %v; want ErrBadToken", err)
	}
	// The name is free again once revoked; a live duplicate is refused.
	if _, _, err := s.Create(session(2), "example-inbox", "Again", "example-inbox"); err != nil {
		t.Fatalf("re-create after revoke: %v", err)
	}
	if _, _, err := s.Create(session(2), "example-inbox", "Twice", "example-inbox"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a second live plugin with one name: %v; want ErrInvalid", err)
	}
}

// TestPluginRowsAreUnderForcedRowLevelSecurity: another owner's plugins and
// messages do not exist for a principal, and a context with no principal sees
// and writes nothing — the database's scoping, not a WHERE clause.
func TestPluginRowsAreUnderForcedRowLevelSecurity(t *testing.T) {
	s, pool, _ := pgStore(t)
	_, p2 := mustCreate(t, s, 2, "example-inbox")
	_, p3 := mustCreate(t, s, 3, "example-inbox")
	m2, _, err := s.Submit(plugin(2), p2.ID, msg("a-1"))
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.List(session(3))
	if err != nil || len(list) != 1 || list[0].ID != p3.ID {
		t.Fatalf("user 3 lists %+v (%v); want only its own plugin", list, err)
	}
	if _, err := s.Message(plugin(3), m2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user 3 reading user 2's message: %v; want ErrNotFound", err)
	}
	// User 3 posting to user 2's plugin id is "no such plugin".
	if _, _, err := s.Submit(plugin(3), p2.ID, msg("x-1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user 3 submitting to user 2's plugin: %v; want ErrNotFound", err)
	}
	// No principal: nothing visible, nothing insertable.
	bare := context.Background()
	for _, table := range []string{"plugins", "plugin_messages"} {
		var n int
		if err := pool.QueryRow(bare, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s with no principal: %d rows (%v); want 0", table, n, err)
		}
	}
	if _, err := pool.Exec(bare, `INSERT INTO plugins (name, label, token_hash, data_scope) VALUES ('x', 'x', 'h', 'x')`); err == nil {
		t.Fatal("an insert with no principal was accepted")
	}
	// FORCE, not just ENABLE: the table owner's own role is held to the policy.
	var forced int
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM pg_class
		WHERE relname IN ('plugins', 'plugin_messages') AND relrowsecurity AND relforcerowsecurity`).Scan(&forced); err != nil || forced != 2 {
		t.Fatalf("tables with FORCED row-level security: %d (%v); want 2", forced, err)
	}
}

func TestADuplicateExternalIDAnswersTheStoredRow(t *testing.T) {
	s, _, _ := pgStore(t)
	_, p := mustCreate(t, s, 2, "example-inbox")
	first, created, err := s.Submit(plugin(2), p.ID, msg("dup-1"))
	if err != nil || !created || first.Status != StatusQueued {
		t.Fatalf("first submit: %+v created=%v err=%v", first, created, err)
	}
	changed := msg("dup-1")
	changed.Body = "a DIFFERENT body for the same id"
	again, created, err := s.Submit(plugin(2), p.ID, changed)
	if err != nil || created || again.ID != first.ID || again.Body != first.Body {
		t.Fatalf("repeat submit: id %d (first %d) created=%v body %q err=%v; want the stored row unchanged",
			again.ID, first.ID, created, again.Body, err)
	}
	// Another plugin may use the same external id: the key is (plugin, external_id).
	_, p2 := mustCreate(t, s, 2, "example-other")
	other, created, err := s.Submit(plugin(2), p2.ID, msg("dup-1"))
	if err != nil || !created || other.ID == first.ID {
		t.Fatalf("same external id on another plugin: %+v created=%v err=%v", other, created, err)
	}
}

// TestConcurrentDuplicatesLandOnce: the plugin-row lock serialises a burst of
// retries of one external id into ONE row.
func TestConcurrentDuplicatesLandOnce(t *testing.T) {
	s, pool, _ := pgStore(t)
	_, p := mustCreate(t, s, 2, "example-inbox")
	var wg sync.WaitGroup
	ids := make([]int64, 8)
	errs := make([]error, 8)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m, _, err := s.Submit(plugin(2), p.ID, msg("race-1"))
			ids[i], errs[i] = m.ID, err
		}(i)
	}
	wg.Wait()
	for i := range ids {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("submit %d: id %d err %v; want every answer to be id %d", i, ids[i], errs[i], ids[0])
		}
	}
	var n int
	if err := pool.QueryRow(plugin(2), `SELECT count(*) FROM plugin_messages WHERE external_id = 'race-1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows for race-1: %d (%v); want 1", n, err)
	}
}

func TestTheCapsAreRefusalsNotTruncations(t *testing.T) {
	s, _, _ := pgStore(t)
	_, p := mustCreate(t, s, 2, "example-inbox")
	big := msg("big")
	big.Body = strings.Repeat("a", MaxBodyBytes+1)
	long := msg("long")
	long.Sender = strings.Repeat("é", MaxSenderRunes+1)
	subj := msg("subj")
	subj.Subject = strings.Repeat("é", MaxSubjectRunes+1)
	for name, in := range map[string]MessageIn{
		"body over 16 KiB":      big,
		"sender over 200 runes": long,
		"subject over 200":      subj,
		"no external id":        {Body: "b"},
		"external id spaced":    {ExternalID: "a b", Body: "b"},
		"blank body":            {ExternalID: "e", Body: "  "},
		"NUL in body":           {ExternalID: "e2", Body: "a\x00b"},
		"invalid utf8":          {ExternalID: "e3", Body: "a\xffb"},
	} {
		if _, _, err := s.Submit(plugin(2), p.ID, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v; want ErrInvalid", name, err)
		}
	}
	// Exactly at each cap is accepted.
	at := MessageIn{ExternalID: "at-cap", Sender: strings.Repeat("é", MaxSenderRunes),
		Subject: strings.Repeat("é", MaxSubjectRunes), Body: strings.Repeat("a", MaxBodyBytes)}
	if _, created, err := s.Submit(plugin(2), p.ID, at); err != nil || !created {
		t.Fatalf("a message exactly at every cap: created=%v err=%v", created, err)
	}
}

// TestTheHourlyRateAndTheQueueCapRefuseButARepeatDoesNot: the 61st new message
// in an hour is refused, a REPEAT of an earlier one is still answered, and 200
// undelivered messages refuse the next even outside the hour.
func TestTheHourlyRateAndTheQueueCapRefuseButARepeatDoesNot(t *testing.T) {
	s, _, dsn := pgStore(t)
	_, p := mustCreate(t, s, 2, "example-inbox")
	for i := 0; i < MaxPerHour; i++ {
		if _, _, err := s.Submit(plugin(2), p.ID, msg("r-"+strconv.Itoa(i))); err != nil {
			t.Fatalf("message %d of %d: %v", i+1, MaxPerHour, err)
		}
	}
	if _, _, err := s.Submit(plugin(2), p.ID, msg("r-over")); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("message %d in an hour: %v; want ErrRateLimited", MaxPerHour+1, err)
	}
	if m, created, err := s.Submit(plugin(2), p.ID, msg("r-0")); err != nil || created || m.ExternalID != "r-0" {
		t.Fatalf("a repeat while rate-limited: created=%v err=%v; want the stored row", created, err)
	}
	// Age every row out of the window and fill the queue to its cap.
	// created_at is immutable (plugin_messages_input_immutable), so the fixture
	// steps around the trigger as the database owner, for this one statement.
	dbtest.OwnerExec(t, dsn, `DO $$ BEGIN
		SET LOCAL session_replication_role = replica;
		UPDATE plugin_messages SET created_at = now() - interval '2 hours';
	END $$`)
	dbtest.OwnerExec(t, dsn, `INSERT INTO plugin_messages (owner_id, plugin_id, external_id, body, created_at)
		SELECT 2, $1, 'q-' || g, 'b', now() - interval '2 hours' FROM generate_series(1, $2) g`, p.ID, MaxQueued-MaxPerHour)
	if _, _, err := s.Submit(plugin(2), p.ID, msg("q-over")); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("message past %d undelivered: %v; want ErrQueueFull", MaxQueued, err)
	}
	// Settling one frees a slot.
	var oldest int64
	ms, _ := s.Queued(plugin(2), p.ID, 1)
	oldest = ms[0].ID
	capChief := mustAgent(t, dsn, 2, "plg-cap").ID
	if ok, err := s.BeginDelivery(plugin(2), oldest, capChief); err != nil || !ok {
		t.Fatalf("begin: %v %v", ok, err)
	}
	if ok, err := s.RecordDelivery(plugin(2), oldest, capChief, StatusDelivered); err != nil || !ok {
		t.Fatalf("settle: %v %v", ok, err)
	}
	if _, created, err := s.Submit(plugin(2), p.ID, msg("q-after")); err != nil || !created {
		t.Fatalf("after one settled: created=%v err=%v", created, err)
	}
}

func TestARevokedPluginCannotSubmit(t *testing.T) {
	s, _, _ := pgStore(t)
	_, p := mustCreate(t, s, 2, "example-inbox")
	if _, err := s.Revoke(session(2), p.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Submit(plugin(2), p.ID, msg("late")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("submit after revoke: %v; want ErrNotFound", err)
	}
}

// mustAgent creates an agent owned by uid, as uid.
func mustAgent(t *testing.T, dsn string, uid int64, name string) agents.Agent {
	t.Helper()
	pool, err := db.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	a, err := agents.NewPG(pool).Create(session(uid), agents.Agent{Name: name, Namespace: "devpod-" + name, DisplayName: "chief",
		Status: agents.StatusRunning, HooksToken: "hooks-" + name})
	if err != nil {
		t.Fatalf("agent %s: %v", name, err)
	}
	return a
}

func TestADeliveryIsRecordedOnceWithItsAuditRow(t *testing.T) {
	s, pool, dsn := pgStore(t)
	_, p := mustCreate(t, s, 2, "example-inbox")
	a := mustAgent(t, dsn, 2, "plg-chief")
	m, _, _ := s.Submit(plugin(2), p.ID, msg("d-1"))
	other := mustAgent(t, dsn, 2, "plg-other")
	if err := s.RecordAttempt(plugin(2), m.ID); err != nil {
		t.Fatal(err)
	}
	// Only a message DELIVERING to the agent settles: not a queued one, and not
	// one delivering to another agent.
	if ok, err := s.RecordDelivery(plugin(2), m.ID, a.ID, StatusDelivered); err != nil || ok {
		t.Fatalf("settling a queued message: settled=%v err=%v; want false", ok, err)
	}
	if ok, err := s.BeginDelivery(plugin(2), m.ID, a.ID); err != nil || !ok {
		t.Fatalf("begin: %v %v", ok, err)
	}
	if ok, err := s.BeginDelivery(plugin(2), m.ID, other.ID); err != nil || ok {
		t.Fatalf("a second begin: %v %v; want false (no longer queued)", ok, err)
	}
	if got, _ := s.Message(plugin(2), m.ID); got.Status != StatusDelivering || got.AgentID == nil || *got.AgentID != a.ID ||
		got.DeliveredAt != nil || got.Attempts != 2 {
		t.Fatalf("a message being delivered = %+v; want delivering to %d, attempt counted", got, a.ID)
	}
	if ok, err := s.RecordDelivery(plugin(2), m.ID, other.ID, StatusDelivered); err != nil || ok {
		t.Fatalf("settling for another agent: settled=%v err=%v; want false", ok, err)
	}
	if ok, err := s.RecordDelivery(plugin(2), m.ID, a.ID, StatusDelivered); err != nil || !ok {
		t.Fatalf("first record: %v %v", ok, err)
	}
	if ok, err := s.RecordDelivery(plugin(2), m.ID, a.ID, StatusUnconfirmed); err != nil || ok {
		t.Fatalf("second record: settled=%v err=%v; want false (already settled)", ok, err)
	}
	got, _ := s.Message(plugin(2), m.ID)
	if got.Status != StatusDelivered || got.Attempts != 2 || got.DeliveredAt == nil || got.AgentID == nil || *got.AgentID != a.ID {
		t.Fatalf("settled message = %+v", got)
	}
	var detail string
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT detail::text FROM audit_log
		WHERE action = 'plugin.delivered' AND object = $1 AND user_id = 2 AND via = 'plugin'`, MessageObject(m.ID)).Scan(&detail); err != nil {
		t.Fatalf("plugin.delivered audit row: %v", err)
	}
	if !strings.Contains(detail, `"outcome": "delivered"`) {
		t.Fatalf("audit detail %s", detail)
	}
	if _, err := s.RecordDelivery(plugin(2), m.ID, a.ID, StatusQueued); !errors.Is(err, ErrInvalid) {
		t.Fatalf("settling as queued: %v; want ErrInvalid", err)
	}
}

// TestTheTriggersHoldUnderTheScopeAllPrincipal: even a principal that sees every
// row cannot rewrite a message's input, point a message at another owner's
// agent, or hang a message off another owner's plugin.
func TestTheTriggersHoldUnderTheScopeAllPrincipal(t *testing.T) {
	s, pool, dsn := pgStore(t)
	sys := authz.System(context.Background())
	_, p := mustCreate(t, s, 2, "example-inbox")
	mine := mustAgent(t, dsn, 2, "plg-mine")
	theirs := mustAgent(t, dsn, 3, "plg-theirs")
	m, _, _ := s.Submit(plugin(2), p.ID, msg("t-1"))

	check := func(name, sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(sys, sql, args...)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || (pg.Code != "23514" && pg.Code != "23503") {
			t.Errorf("%s: err = %v; want a check or foreign-key violation", name, err)
		}
	}
	for _, col := range []string{"body = 'rewritten'", "sender = 'x'", "subject = 'x'", "external_id = 'x'", "received_at = now()"} {
		check("message "+col, `UPDATE plugin_messages SET `+col+` WHERE id = $1`, m.ID)
	}
	check("message to another owner's agent", `UPDATE plugin_messages SET agent_id = $2 WHERE id = $1`, m.ID, theirs.ID)
	check("message on another owner's plugin", `INSERT INTO plugin_messages (owner_id, plugin_id, external_id, body) VALUES (3, $1, 'z', 'b')`, p.ID)

	// What MAY change does: naming the owner's own agent, and that agent's
	// deletion nulling agent_id.
	if _, err := pool.Exec(sys, `UPDATE plugin_messages SET status = 'delivering', agent_id = $2 WHERE id = $1`, m.ID, mine.ID); err != nil {
		t.Fatalf("naming the owner's own agent: %v", err)
	}
	if _, err := pool.Exec(sys, `DELETE FROM agents WHERE id = $1`, mine.ID); err != nil {
		t.Fatalf("deleting the agent a message names: %v", err)
	}
	var stillThere int
	if err := pool.QueryRow(sys, `SELECT count(*) FROM plugin_messages WHERE id = $1 AND agent_id IS NULL`, m.ID).Scan(&stillThere); err != nil || stillThere != 1 {
		t.Fatalf("the message after its agent's deletion: %d (%v); want kept with agent_id NULL", stillThere, err)
	}
}

// TestAnAbandonedDeliveryIsQueuedWithNoAgentAndADeadOneIsRecoveredUnconfirmed
// pins the two exits from `delivering` that are not the turn's own outcome:
// AbandonDelivery (the turn provably sent nothing) returns the message to the
// queue with its agent CLEARED — and a queued row that names an agent is refused
// by the schema itself — and RecoverDeliveries settles a row a dead process left
// `delivering` to THAT agent as unconfirmed (audited), leaving another agent's alone.
func TestAnAbandonedDeliveryIsQueuedWithNoAgentAndADeadOneIsRecoveredUnconfirmed(t *testing.T) {
	s, pool, dsn := pgStore(t)
	sys := authz.System(context.Background())
	_, p := mustCreate(t, s, 2, "example-inbox")
	a := mustAgent(t, dsn, 2, "plg-a")
	b := mustAgent(t, dsn, 2, "plg-b")
	m1, _, _ := s.Submit(plugin(2), p.ID, msg("x-1"))
	m2, _, _ := s.Submit(plugin(2), p.ID, msg("x-2"))
	m3, _, _ := s.Submit(plugin(2), p.ID, msg("x-3"))
	for _, c := range []struct {
		m  Message
		to int64
	}{{m1, a.ID}, {m2, a.ID}, {m3, b.ID}} {
		if ok, err := s.BeginDelivery(plugin(2), c.m.ID, c.to); err != nil || !ok {
			t.Fatalf("begin %d: %v %v", c.m.ID, ok, err)
		}
	}
	// Abandon for the wrong agent does nothing; for the right one, queued + no agent.
	if err := s.AbandonDelivery(plugin(2), m1.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Message(plugin(2), m1.ID); got.Status != StatusDelivering {
		t.Fatalf("abandoned by another agent: %+v; want still delivering", got)
	}
	if err := s.AbandonDelivery(plugin(2), m1.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Message(plugin(2), m1.ID); got.Status != StatusQueued || got.AgentID != nil || got.Attempts != 1 {
		t.Fatalf("abandoned: %+v; want queued, no agent, the attempt still counted", got)
	}
	var pg *pgconn.PgError
	if _, err := pool.Exec(sys, `UPDATE plugin_messages SET agent_id = $2 WHERE id = $1`, m1.ID, a.ID); !errors.As(err, &pg) ||
		pg.ConstraintName != "plugin_messages_queued_has_no_agent" {
		t.Fatalf("a queued message naming an agent: %v; want plugin_messages_queued_has_no_agent", err)
	}
	// Recovery is per agent: a's dead delivery settles, b's live one does not.
	if n, err := s.RecoverDeliveries(plugin(2), a.ID); err != nil || n != 1 {
		t.Fatalf("recover a: %d %v; want 1", n, err)
	}
	if got, _ := s.Message(plugin(2), m2.ID); got.Status != StatusUnconfirmed || got.DeliveredAt == nil {
		t.Fatalf("recovered: %+v; want unconfirmed", got)
	}
	if got, _ := s.Message(plugin(2), m3.ID); got.Status != StatusDelivering {
		t.Fatalf("another agent's delivery: %+v; want untouched", got)
	}
	var detail string
	if err := pool.QueryRow(sys, `SELECT detail::text FROM audit_log WHERE action = 'plugin.delivered' AND object = $1`,
		MessageObject(m2.ID)).Scan(&detail); err != nil || !strings.Contains(detail, `"recovered": true`) ||
		!strings.Contains(detail, `"outcome": "unconfirmed"`) {
		t.Fatalf("the recovery's audit row: %q (%v)", detail, err)
	}
}
