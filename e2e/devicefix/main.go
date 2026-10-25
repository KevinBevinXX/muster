// Command devicefix registers a paired phone's push endpoint for the browser
// suite (e2e/tests/push.spec.ts), as PUT /api/device/push does once its test
// push is accepted:
//
//	DATABASE_URL=…_e2e go run ./e2e/devicefix push <device-id> <endpoint>
//
// ⚠ WHY NOT THE ROUTE ITSELF: PUT /api/device/push keeps a registration only
// after muster has sent the endpoint a VAPID test push and got a 2xx, and the
// endpoint must be an https URL on the default port of an allowed push host.
// The e2e harness has no such service, so this command makes the SAME store
// write the route makes after its probe (push.PGStore.ClaimNative, owner and
// device from the device row). The route, probe included, is
// internal/api's TestTheDeviceListNoticesAPhonesPushRegistration.
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e" (e2edb.Check).
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/e2e/internal/e2edb"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/push"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "devicefix:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	if len(args) != 3 || args[0] != "push" {
		return fmt.Errorf("usage: devicefix push <device-id> <endpoint>")
	}
	id, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return fmt.Errorf("device id: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	// The device's owner, read as pluginfix reads (row-level security's scope
	// set to all on a fixture connection); the claim itself is the store's.
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `SELECT set_config('muster.scope', 'all', false)`); err != nil {
		return err
	}
	var owner int64
	var name string
	if err := conn.QueryRow(ctx, `SELECT owner_id, name FROM devices WHERE id = $1`, id).Scan(&owner, &name); err != nil {
		return fmt.Errorf("device %d: %w", id, err)
	}
	var sub push.Subscription
	sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth = args[2], "BStubClientKeyForE2E", "stubauthsecret"
	return push.NewPG(pool).WithAllowlist(push.NewAllowlist(nil)).ClaimNative(ctx, owner, id, sub, "muster app: "+name)
}
