package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/gotrue/gotruetest"
	"github.com/ZacxDev/muster/internal/users"
)

var accountsSecret = strings.Repeat("s", 40)

// validGoTrueConfig is a gotrue-mode config that validate accepts.
func validGoTrueConfig() config {
	return config{
		Database:     "postgres://x/y",
		Port:         8105,
		AuthMode:     "gotrue",
		GoTrueURL:    "http://auth.example.invalid:9999",
		GoTrueSecret: accountsSecret,
		HookToken:    strings.Repeat("h", 40),
		BrowserPort:  8106,
		Standalone:   true,
		RouterActor:  defaultRouterActor,
	}
}

// TestGoTrueModeRefusesToBootUnlessEveryPreconditionHolds is the boot
// refusal matrix: each precondition removed ON ITS OWN from a config that
// boots, and each refusal names its own variable — so no refusal is masked by
// an earlier one.
func TestGoTrueModeRefusesToBootUnlessEveryPreconditionHolds(t *testing.T) {
	if err := validGoTrueConfig().validate(); err != nil {
		t.Fatalf("positive control: the complete gotrue config is refused: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mut    func(*config)
		naming string
	}{
		{"no GoTrue URL", func(c *config) { c.GoTrueURL = "" }, envGoTrueURL},
		{"no secret", func(c *config) { c.GoTrueSecret = "" }, envGoTrueSecret},
		{"neither half", func(c *config) { c.GoTrueURL, c.GoTrueSecret = "", "" }, envGoTrueURL},
		{"short secret", func(c *config) { c.GoTrueSecret = strings.Repeat("s", 31) }, envGoTrueSecret},
		{"non-http URL", func(c *config) { c.GoTrueURL = "ftp://auth" }, envGoTrueURL},
		{"unarmed hook tier", func(c *config) { c.HookToken = "" }, envHookToken},
		{"no browser listener", func(c *config) { c.BrowserPort = 0 }, envBrowserPort},
		{"browser port = main port", func(c *config) { c.BrowserPort = c.Port }, envBrowserPort},
		{"unknown mode", func(c *config) { c.AuthMode = "oauth" }, envAuthMode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validGoTrueConfig()
			tc.mut(&c)
			err := c.validate()
			if err == nil {
				t.Fatalf("%s: booted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.naming) {
				t.Fatalf("%s: the refusal does not name %s: %v", tc.name, tc.naming, err)
			}
		})
	}
	// Password mode: none of the gotrue preconditions apply — except the pair
	// and the secret length whenever GoTrue is half-configured, since there it
	// arms the admin claim.
	pw := validGoTrueConfig()
	pw.AuthMode, pw.GoTrueURL, pw.GoTrueSecret, pw.HookToken, pw.BrowserPort = "password", "", "", "", 0
	if err := pw.validate(); err != nil {
		t.Fatalf("password mode with nothing gotrue set: %v", err)
	}
	pw.GoTrueURL = "http://auth.example.invalid:9999"
	if err := pw.validate(); err == nil || !strings.Contains(err.Error(), envGoTrueSecret) {
		t.Fatalf("password mode with half a GoTrue pair: %v", err)
	}
	pw.GoTrueSecret = accountsSecret
	if err := pw.validate(); err != nil {
		t.Fatalf("password mode with GoTrue configured (the claim step): %v", err)
	}
}

// TestOnlyTheBrowserListenerServesGoTrueSignIn drives the binary's own
// assembly and listen path: the same gotrue-mode server answers the sign-in
// form on MUSTER_BROWSER_PORT and refuses it on the main port, while the hook
// token is admitted on the main port.
func TestOnlyTheBrowserListenerServesGoTrueSignIn(t *testing.T) {
	fake := gotruetest.New(t, []byte(accountsSecret))
	cfg := testConfig()
	cfg.Database = dbtest.DSN(t)
	cfg.AuthMode = "gotrue"
	cfg.GoTrueURL = fake.URL
	cfg.GoTrueSecret = accountsSecret
	cfg.HookToken = strings.Repeat("h", 40)
	cfg.BrowserPort = freePort(t)
	if err := cfg.validate(); err != nil {
		t.Fatalf("fixture config refused: %v", err)
	}
	main := startServer(t, cfg)
	browser := "http://127.0.0.1:" + strconv.Itoa(cfg.BrowserPort)

	get := func(url string, hdr map[string]string) (int, string) {
		req, _ := http.NewRequest("GET", url, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(browser+"/login", nil); code != http.StatusOK || !strings.Contains(body, `name="email"`) {
		t.Fatalf("browser listener /login: %d", code)
	}
	if code, body := get(main+"/login", nil); code != http.StatusForbidden || strings.Contains(body, `name="email"`) {
		t.Fatalf("main listener /login: %d, want 403 with no form", code)
	}
	code, _ := get(main+"/api/tags", map[string]string{"X-Muster-Token": cfg.HookToken})
	if code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusServiceUnavailable {
		t.Fatalf("the hook token on the main listener: %d", code)
	}
}

// TestTheClaimAdminCommandPrintsASingleLiveLink runs the real subcommand
// against Postgres.
func TestTheClaimAdminCommandPrintsASingleLiveLink(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	reset := func() {
		sys := authz.System(context.Background())
		_, _ = pool.Exec(sys, `DELETE FROM invites WHERE claim_user_id = 1`)
		_, _ = pool.Exec(sys, `UPDATE users SET gotrue_sub = NULL, email = NULL, disabled_at = NULL WHERE id = 1`)
	}
	reset()
	t.Cleanup(reset)
	env := map[string]string{
		envDatabase: dsn, envPublicURL: "https://muster.example.com",
		envGoTrueURL: "http://auth.example.invalid:9999", envGoTrueSecret: accountsSecret,
	}
	getenv := func(k string) string { return env[k] }
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		rc := runAccountCommand(ctx, args, getenv, &out, &errb)
		return rc, strings.TrimSpace(out.String()), errb.String()
	}
	rc, first, stderr := run("invite", "--claim-admin")
	if rc != 0 || !strings.HasPrefix(first, "https://muster.example.com/auth/claim#") {
		t.Fatalf("claim-admin: rc=%d out=%q err=%q", rc, first, stderr)
	}
	rc, second, _ := run("invite", "--claim-admin")
	if rc != 0 || second == first {
		t.Fatalf("second claim-admin: rc=%d", rc)
	}
	store := users.NewPG(pool)
	live := func(link string) bool {
		tok := link[strings.LastIndex(link, "#")+1:]
		sum := sha256.Sum256([]byte(tok))
		ok, err := store.ClaimInviteLive(ctx, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if live(first) || !live(second) {
		t.Fatalf("after two mints: first live=%v second live=%v, want false/true", live(first), live(second))
	}
	// Without GoTrue it refuses rather than printing a link nobody can redeem.
	delete(env, envGoTrueURL)
	delete(env, envGoTrueSecret)
	if rc, out, _ := run("invite", "--claim-admin"); rc == 0 || out != "" {
		t.Fatalf("claim-admin with no GoTrue: rc=%d out=%q", rc, out)
	}
	if rc, _, _ := run("invite"); rc != 2 {
		t.Fatalf("bad usage: rc=%d, want 2", rc)
	}
}

// TestTheUserDisableCommandSetsTheRowAndBansTheLogin runs `user disable` and
// `user enable` against Postgres and the fake GoTrue.
func TestTheUserDisableCommandSetsTheRowAndBansTheLogin(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	fake := gotruetest.New(t, []byte(accountsSecret))
	sub := fake.AddUser("m@example.com", "a-long-enough-password")
	sys := authz.System(ctx)
	var id int64
	if err := pool.QueryRow(sys, `INSERT INTO users (role, gotrue_sub) VALUES ('member', $1) RETURNING id`, sub).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(authz.System(context.Background()), `DELETE FROM users WHERE id = $1`, id) })
	env := map[string]string{envDatabase: dsn, envGoTrueURL: fake.URL, envGoTrueSecret: accountsSecret}
	var out, errb bytes.Buffer
	if rc := runAccountCommand(ctx, []string{"user", "disable", strconv.FormatInt(id, 10)}, func(k string) string { return env[k] }, &out, &errb); rc != 0 {
		t.Fatalf("disable: rc=%d %s", rc, errb.String())
	}
	u, err := users.NewPG(pool).ByID(ctx, id)
	if err != nil || !u.Disabled() {
		t.Fatalf("row not disabled: %+v %v", u, err)
	}
	if fu, _ := fake.User(sub); !fu.Banned {
		t.Fatal("GoTrue user not banned")
	}
	if rc := runAccountCommand(ctx, []string{"user", "enable", strconv.FormatInt(id, 10)}, func(k string) string { return env[k] }, &out, &errb); rc != 0 {
		t.Fatalf("enable: rc=%d %s", rc, errb.String())
	}
	if fu, _ := fake.User(sub); fu.Banned {
		t.Fatal("GoTrue user still banned after enable")
	}
}

// TestAnMstPrefixedSharedSecretIsRefusedAtBoot (accounts PR 6, review round
// 1): an `mst_` value on the machine tier is read as a personal API token, so
// a MUSTER_HOOK_TOKEN or MUSTER_SERVICE_TOKEN in that namespace would be
// refused 401 on every request. Boot refuses it instead, naming the variable.
// The control is the unprefixed fixture, which boots — including with a
// service token set (so the service case is reached, not masked).
func TestAnMstPrefixedSharedSecretIsRefusedAtBoot(t *testing.T) {
	ok := validGoTrueConfig()
	ok.ServiceToken = strings.Repeat("s", 40)
	if err := ok.validate(); err != nil {
		t.Fatalf("CONTROL: the unprefixed config is refused: %v", err)
	}
	for name, mutate := range map[string]func(*config){
		"MUSTER_HOOK_TOKEN":    func(c *config) { c.HookToken = "mst_" + strings.Repeat("h", 43) },
		"MUSTER_SERVICE_TOKEN": func(c *config) { c.ServiceToken = "mst_" + strings.Repeat("s", 43) },
	} {
		c := ok
		mutate(&c)
		err := c.validate()
		if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "mst_") {
			t.Errorf("an mst_-prefixed %s: validate() = %v; want a refusal naming it", name, err)
		}
	}
}
