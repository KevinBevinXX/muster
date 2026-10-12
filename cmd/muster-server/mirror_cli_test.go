package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/mirror"
)

// TestTheMirrorTokenCLIMintsListsAndRevokes: `muster-server mirror token
// create` prints the token ONCE on stdout (and only the token, so it can be
// piped into a file), `ls` never prints it, and `revoke` makes it stop
// authenticating. The token is OWNED BY THE OPERATOR (user 1).
func TestTheMirrorTokenCLIMintsListsAndRevokes(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == envDatabase {
			return dsn
		}
		return ""
	}
	cli := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		rc := runMirrorCLI(ctx, args, getenv, &out, &errb)
		return rc, out.String(), errb.String()
	}
	var b [3]byte
	_, _ = rand.Read(b[:])
	host := "cli-" + hex.EncodeToString(b[:])

	rc, out, errs := cli("token", "create", host)
	tok := strings.TrimSpace(out)
	if rc != 0 || !strings.HasPrefix(tok, mirror.TokenPrefix) || strings.Count(out, "\n") != 1 {
		t.Fatalf("create: rc %d stdout %q stderr %q; want exactly one token line", rc, out, errs)
	}
	store := mirror.NewPG(pool)
	rec, err := store.Authenticate(authz.System(ctx), tok)
	if err != nil || rec.Host != host || rec.OwnerID != authz.OperatorUserID {
		t.Fatalf("the minted token does not authenticate as %s: %+v %v", host, rec, err)
	}

	rc, out, _ = cli("token", "ls")
	if rc != 0 || !strings.Contains(out, host) || strings.Contains(out, tok) {
		t.Fatalf("ls: rc %d; it must name the host and never print the token:\n%s", rc, out)
	}

	if rc, _, errs := cli("token", "revoke", "not-a-number"); rc != 2 {
		t.Fatalf("revoke with a bad id: rc %d (%s), want 2", rc, errs)
	}
	if rc, _, errs := cli("token", "revoke", strconv.FormatInt(rec.ID, 10)); rc != 0 {
		t.Fatalf("revoke: rc %d %s", rc, errs)
	}
	if _, err := store.Authenticate(authz.System(ctx), tok); err == nil {
		t.Fatal("a revoked token still authenticates")
	}
	if rc, _, _ := cli("token", "revoke", strconv.FormatInt(rec.ID, 10)); rc != 1 {
		t.Fatal("revoking twice did not report the token as already revoked")
	}
	if rc, _, _ := cli("nonsense"); rc != 2 {
		t.Fatalf("an unknown verb: rc %d, want 2", rc)
	}
	if rc, _, _ := cli("token", "create", "Bad_Host"); rc != 1 {
		t.Fatalf("a bad host: rc %d, want 1", rc)
	}
}
