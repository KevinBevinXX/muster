package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/sse"
	"github.com/ZacxDev/muster/internal/users"
)

// TestSeamPersonalTokensThroughTheRealServer drives `whoami` and `token …` at
// the real api.Handler over real Postgres, with the credential in the
// ENVIRONMENT (not --token), so the variable names are under test end to end:
// MUSTER_TOKEN, and MUSTER_HOOK_TOKEN as its alias.
func TestSeamPersonalTokensThroughTheRealServer(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatal(err)
	}
	sys := authz.System(ctx)
	var uid int64
	if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', 'cli-seam') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = pool.Exec(bg, `DELETE FROM api_tokens WHERE user_id = $1`, uid)
		_, _ = pool.Exec(bg, `DELETE FROM audit_log WHERE user_id = $1`, uid)
		_, _ = pool.Exec(bg, `DELETE FROM users WHERE id = $1`, uid)
	})
	// Tokens are minted by the BROWSER session only (/account/tokens) — there
	// is no CLI or API mint — so this test mints its two through the store,
	// as that page does.
	asUser := authz.WithPrincipal(ctx, authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
	first, _, err := apitokens.NewPG(pool).Create(asUser, "browser-minted")
	if err != nil {
		t.Fatal(err)
	}
	secondTok, secondRec, err := apitokens.NewPG(pool).Create(asUser, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	created := struct {
		ID    int64
		Token string
	}{secondRec.ID, secondTok}

	srv := api.New(sse.New(8), api.AuthConfig{HookToken: seamHookToken}, log.New(io.Discard, "", 0))
	srv.UseExtensions(api.Extensions{Notes: notes.NewPG(pool), SessionLiveness: seamLiveness{},
		Users: users.NewPG(pool), APITokens: apitokens.NewPG(pool)})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	cli := func(env map[string]string, args ...string) (int, string, string) {
		t.Helper()
		var out, errOut strings.Builder
		env["MUSTER_API_URL"] = ts.URL
		full := append([]string{"--env-file", filepath.Join(t.TempDir(), "absent.env")}, args...)
		code := run(context.Background(), testProgName, full, strings.NewReader(""), &out, &errOut,
			func(k string) string { return env[k] }, t.TempDir())
		return code, out.String(), errOut.String()
	}
	who := func(env map[string]string) (int, map[string]any) {
		t.Helper()
		code, out, _ := cli(env, "whoami")
		var m map[string]any
		_ = json.Unmarshal([]byte(out), &m)
		return code, m
	}

	// whoami: the personal token is its owner; the hook secret the machine user.
	if code, m := who(map[string]string{"MUSTER_TOKEN": first}); code != exitOK || m["userId"] != float64(uid) || m["via"] != "api-token" {
		t.Fatalf("whoami with a personal token in MUSTER_TOKEN: exit %d %v", code, m)
	}
	if code, m := who(map[string]string{"MUSTER_HOOK_TOKEN": seamHookToken}); code != exitOK || m["userId"] != float64(1) || m["via"] != "hook" {
		t.Fatalf("whoami with the hook token: exit %d %v", code, m)
	}

	// There is no `token create`: the CLI cannot mint (review round 1).
	if code, _, _ := cli(map[string]string{"MUSTER_TOKEN": first}, "token", "create", "--name", "x"); code == exitOK {
		t.Fatal("`token create` succeeded; minting is browser-session only")
	}
	// It works through the ALIAS name too, and acts as the same user.
	if code, m := who(map[string]string{"MUSTER_HOOK_TOKEN": created.Token}); code != exitOK || m["userId"] != float64(uid) {
		t.Fatalf("the new token via MUSTER_HOOK_TOKEN: exit %d %v", code, m)
	}
	// token ls: both, by name, and never a value.
	code, out, _ := cli(map[string]string{"MUSTER_TOKEN": first}, "token", "ls")
	if code != exitOK || !strings.Contains(out, "laptop") || !strings.Contains(out, "browser-minted") ||
		strings.Contains(out, created.Token) || strings.Contains(out, first) {
		t.Fatalf("token ls: exit %d %s", code, out)
	}
	// The hook secret cannot manage tokens: exit 3.
	if code, _, errOut := cli(map[string]string{"MUSTER_HOOK_TOKEN": seamHookToken}, "token", "ls"); code != exitAuth {
		t.Fatalf("token ls with the hook token: exit %d (%s); want %d", code, errOut, exitAuth)
	}
	// token revoke: immediate.
	if code, out, errOut := cli(map[string]string{"MUSTER_TOKEN": first}, "token", "revoke", strconv.FormatInt(created.ID, 10)); code != exitOK || !strings.Contains(out, `"revoked": true`) {
		t.Fatalf("token revoke: exit %d %s %s", code, out, errOut)
	}
	if code, _ := who(map[string]string{"MUSTER_TOKEN": created.Token}); code != exitAuth {
		t.Fatalf("whoami with the revoked token: exit %d; want %d", code, exitAuth)
	}
	// An id that is not yours (or not anyone's) is not found: exit 4.
	if code, _, _ := cli(map[string]string{"MUSTER_TOKEN": first}, "token", "revoke", "999999999"); code != exitNotFound {
		t.Fatalf("revoking an unknown id: exit %d; want %d", code, exitNotFound)
	}
	if code, _, _ := cli(map[string]string{"MUSTER_TOKEN": first}, "token", "revoke", "abc"); code != exitUsage {
		t.Fatalf("revoking a non-numeric id: exit %d; want %d", code, exitUsage)
	}
}
