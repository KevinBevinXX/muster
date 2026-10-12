package db

import (
	"context"
	"fmt"
	"log"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
)

// This file is where a request's authz.Principal becomes something Postgres can
// enforce (accounts PR 3a). Migration 0007 puts row-level security on every
// owned table, keyed on two custom settings:
//
//	muster.uid    the acting user's id ('' = nobody)
//	muster.scope  'all' for authz.System (and the migration runner), else ''
//
// 🔴 THEY ARE SET ON EVERY ACQUIRE — pgxpool's PrepareConn, which every pool
// operation goes through (Exec, Query, QueryRow, SendBatch, CopyFrom, Begin and
// Acquire itself) — AND THEY ARE ALWAYS OVERWRITTEN, never "set if changed". A
// pooled connection last used by user A must never carry A's id into user B's
// query, and a cache of "what this connection already has" is exactly the state
// that would let it. AfterRelease clears them as well, so an idle connection
// holds nobody.
//
// 🔴 THE ROLE IS SET ON EVERY ACQUIRE TOO. Production's login role is a
// SUPERUSER, and a superuser bypasses row-level security unconditionally —
// FORCE included — so without this switch every policy is inert. AppRole is
// NOLOGIN, NOSUPERUSER, NOBYPASSRLS (0007's header has the measurement).
//
// 🔴 FAIL CLOSED. A context with no principal gets uid '' and scope '': the
// policies then match no row, and owner_id's default (the uid setting) is NULL,
// so every insert into an owned table is refused (a NULL owner satisfies no
// policy's WITH CHECK: SQLSTATE 42501). It is reported (once per
// call site) by reportUnscoped, because "the board is empty" is otherwise
// indistinguishable from "the board has nothing on it".

// AppRole is the Postgres role every application connection acts as. Created
// by migration 0007.
const AppRole = "muster_app"

// Setting names. Spelled once; migration 0007 spells them in SQL.
const (
	settingUID   = "muster.uid"
	settingScope = "muster.scope"
	scopeAll     = "all"
)

// migratorKey marks the migration runner's own context: it must keep the login
// role (DDL) and see every row. Unexported, so only this package can set it.
// The value is the muster.uid to set: ” for Migrate (a migration that inserts
// a row with no owner must FAIL, not land as somebody); this package's tests
// set "1" so raw schema fixtures can rely on owner_id's default.
type migratorKey struct{}

func asMigrator(ctx context.Context) context.Context {
	return context.WithValue(ctx, migratorKey{}, "")
}

// pingKey marks a liveness ping: it acts as AppRole with NO user — it reads
// nothing, so it needs nobody — but is not reported as an unscoped query.
type pingKey struct{}

// Ping checks the pool can hand out a connection AS THE APPLICATION ROLE, which
// is a stronger readiness answer than a bare ping: it fails if migration 0007's
// role is missing, which would fail every request. It is the readiness check
// cmd/muster-server installs.
func Ping(ctx context.Context, pool interface {
	Ping(context.Context) error
}) error {
	return pool.Ping(context.WithValue(ctx, pingKey{}, true))
}

// scopeSettings is what one acquire under ctx sets: the role, muster.uid and
// muster.scope. It is a pure function of the context so the mapping is
// testable without a database.
func scopeSettings(ctx context.Context) (role, uid, scope string, scoped bool) {
	if uid, ok := ctx.Value(migratorKey{}).(string); ok {
		// 'none' is RESET ROLE: back to the session (login) user.
		return "none", uid, scopeAll, true
	}
	p, ok := authz.From(ctx)
	if !ok {
		if ping, _ := ctx.Value(pingKey{}).(bool); ping {
			return AppRole, "", "", true
		}
		return AppRole, "", "", false
	}
	if p.All() {
		return AppRole, "", scopeAll, true
	}
	if p.UserID <= 0 {
		// authz.WithPrincipal refuses this shape; a zero-user non-System
		// principal is still nobody, never everybody.
		return AppRole, "", "", false
	}
	return AppRole, strconv.FormatInt(p.UserID, 10), "", true
}

// applyScopeSQL sets all three in ONE round trip. set_config(..., false) is
// session-level, which is what a pooled connection needs: the value must hold
// for every statement until the next acquire overwrites it. (A
// transaction-local value would vanish at the first COMMIT inside the
// acquire.)
const applyScopeSQL = `SELECT set_config('role', $1, false), set_config('` + settingUID + `', $2, false), set_config('` + settingScope + `', $3, false)`

// prepareConn is the pool's PrepareConn hook.
//
// An error DESTROYS the connection and fails the caller's query (returning
// false, err): a connection whose scope could not be set must not serve
// anything, and a retry on another connection could mask a real defect (the
// role missing because 0007 never ran).
func prepareConn(ctx context.Context, conn *pgx.Conn) (bool, error) {
	role, uid, scope, scoped := scopeSettings(ctx)
	if !scoped {
		reportUnscoped()
	}
	if _, err := conn.Exec(ctx, applyScopeSQL, role, uid, scope); err != nil {
		return false, fmt.Errorf("db: set connection scope (role %s): %w", role, err)
	}
	return true, nil
}

// releaseResetTimeout bounds afterRelease's reset. It runs on its own
// goroutine (pgxpool calls AfterRelease asynchronously), so it never delays
// the releasing caller.
const releaseResetTimeout = 5 * time.Second

// afterRelease is the pool's AfterRelease hook: it clears the scope so an idle
// connection holds nobody. A connection that cannot be reset is DESTROYED
// rather than returned to the pool with someone's id still on it.
func afterRelease(conn *pgx.Conn) bool {
	ctx, cancel := context.WithTimeout(context.Background(), releaseResetTimeout)
	defer cancel()
	_, err := conn.Exec(ctx, applyScopeSQL, AppRole, "", "")
	return err == nil
}

var unscopedHook atomic.Pointer[func(caller string)]

// SetUnscopedHook installs f to be called with the caller's function name every
// time a connection is acquired under a context with NO principal, and returns
// a func that restores the previous hook. Tests use it to assert a code path is
// scoped; production installs none and gets one log line per distinct call
// site instead.
func SetUnscopedHook(f func(caller string)) (restore func()) {
	prev := unscopedHook.Swap(&f)
	return func() { unscopedHook.Store(prev) }
}

var unscopedSeen sync.Map

// reportUnscoped names the first frame outside pgx and this package.
func reportUnscoped() {
	caller := unscopedCaller()
	if h := unscopedHook.Load(); h != nil && *h != nil {
		(*h)(caller)
	}
	if _, dup := unscopedSeen.LoadOrStore(caller, true); !dup {
		log.Printf("muster: db: a query ran with NO principal (from %s): row-level security "+
			"returns no rows to it and refuses its inserts. A request path must come through a "+
			"require* wrapper; a background loop must use authz.System.", caller)
	}
}

func unscopedCaller() string {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(3, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		fn := f.Function
		if fn != "" && !strings.HasPrefix(fn, "github.com/jackc/") &&
			!strings.HasPrefix(fn, "github.com/ZacxDev/muster/internal/db.") &&
			!strings.HasPrefix(fn, "runtime.") {
			return fn
		}
		if !more {
			return "unknown"
		}
	}
}
