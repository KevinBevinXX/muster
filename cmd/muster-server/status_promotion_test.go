package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestAStartedAgentsStoredStatusFollowsItsReadyInstance is the stuck-
// `provisioning` report, driven through the binary's own wiring: an agent is
// Started through the same route the UI's Start button posts to, its instance
// comes up ready, and the STORED status — what GET /api/agents and
// `muster agent ls` report — must reach `running`.
//
// 🔴 IT FAILS ON THE TREE BEFORE internal/agentstatus, FOR BOTH KINDS. Start
// stores `provisioning` for an agent that was never kicked off and nothing in
// the process ever wrote `running` afterwards (the kickoff deliverer stamps
// kicked_off, never a status; agents.ActionPersistStatus had no driver), so the
// machine read said `provisioning` for as long as the agent lived.
//
// 🔴 THE CONTROL IS WHAT MAKES THE RED MEAN SOMETHING. The noop driver reports a
// created instance ready, and the HTML status route composes the status LIVE
// (agents.ComputeStatus), so `running` there proves the instance really is ready
// — the rival explanation for a stuck `provisioning` ("the instance never came
// up") is ruled out by the same run, and only the stored column is left.
func TestAStartedAgentsStoredStatusFollowsItsReadyInstance(t *testing.T) {
	cfg := ccTestConfig()
	cfg.Database = dbtest.DSN(t)
	cfg.Port = 0
	cfg.Standalone = true
	cfg.RouterActor = defaultRouterActor
	cfg.UIPassword = "status-promotion-operator-password-0123456789"
	cfg.HookToken = "status-promotion-hook-token-not-a-real-one-0123"
	base := startServer(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, cfg.Database)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	store := agents.NewPG(pool)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	resp, err := client.PostForm(base+"/login", url.Values{"password": {cfg.UIPassword}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if u, _ := url.Parse(base); len(jar.Cookies(u)) == 0 {
		t.Fatalf("instrument check FAILED: login issued no session (status %d)", resp.StatusCode)
	}

	suffix := time.Now().Format("150405.000000")
	suffix = strings.ReplaceAll(suffix, ".", "")
	for _, c := range []struct{ kind, name, account string }{
		{agents.KindGateway, "promo-gw-" + suffix, ""},
		{agents.KindClaudeCode, "promo-cc-" + suffix, "work"},
	} {
		t.Run(c.kind, func(t *testing.T) {
			// A SAVED agent: stored `stopped`, nothing provisioned — exactly what
			// "Save for later" leaves, and what Start then brings up.
			row, err := store.Create(ctx, agents.Agent{
				Name: c.name, DisplayName: c.name, Kind: c.kind, CCAccount: c.account,
				Namespace: agents.NamespaceFor(cfg.AgentNamespacePrefix, c.name),
				Status:    agents.StatusStopped,
			})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agents WHERE id=$1`, row.ID) })

			req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/agents/%d/start", base, row.ID), nil)
			req.Header.Set("Origin", base)
			req.Header.Set("HX-Request", "true")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				t.Fatalf("POST /agents/%d/start = %d: %s", row.ID, resp.StatusCode, body)
			}

			var stored, live string
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				stored = apiStatus(t, base, cfg.HookToken, c.name)
				live = liveStatusHTML(t, client, base, row.ID)
				if stored == agents.StatusRunning {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			// The control: the instance IS ready, by the live composition.
			if live != agents.StatusRunning {
				t.Fatalf("control FAILED: the live status route reads %q, want running — the instance "+
					"never came up, so this run cannot say anything about the stored status", live)
			}
			if stored != agents.StatusRunning {
				t.Fatalf("GET /api/agents reports %s agent %q as %q 15s after Start, while its instance "+
					"is ready (the live status route reads %q): the stored status never left "+
					"provisioning", c.kind, c.name, stored, live)
			}
		})
	}
}

// apiStatus reads one agent's STORED status from the machine list.
func apiStatus(t *testing.T, base, token, name string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/api/agents", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /api/agents = %d: %s", resp.StatusCode, b)
	}
	var rows []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r.Status
		}
	}
	t.Fatalf("GET /api/agents has no row %q", name)
	return ""
}

// liveStatusHTML reads the status the UI's status icon composes LIVE, from its
// aria-label ("Status: <status>").
func liveStatusHTML(t *testing.T, client *http.Client, base string, id int64) string {
	t.Helper()
	resp, err := client.Get(fmt.Sprintf("%s/ui/agents/%d/status", base, id))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for _, s := range []string{agents.StatusRunning, agents.StatusProvisioning, agents.StatusStopped, agents.StatusError, agents.StatusPending} {
		if strings.Contains(string(b), `aria-label="Status: `+s+`"`) {
			return s
		}
	}
	return "unparsed: " + string(b)
}
