package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ZacxDev/muster/internal/podagent"
)

// useAgentHome makes hermesd's own HOME the agent shell's (see serve).
func useAgentHome(cfg config) error { return os.Setenv("HOME", cfg.agentHome()) }

// serve runs hermesd: seed, muster's wire, git routing, the checkout, then the
// supervised gateway — until SIGTERM, which is forwarded to the gateway (it
// drains for 30 s and exits 1, which is not a crash) before hermesd exits.
func serve() error {
	cfg, err := configFromEnv()
	if err != nil {
		return err
	}
	bearer, err := podagent.ResolveBearer("hermesd", os.Getenv("HOOKS_TOKEN"), os.Getenv("MUSTER_GATEWAY_BEARER"))
	if err != nil {
		return err
	}
	apiKey, err := seed(cfg)
	if err != nil {
		return err
	}
	// 🔴 hermesd's OWN HOME IS THE AGENT SHELL'S HOME, so the git routing it
	// writes with `git config --global` (podagent.SyncGitRouting) lands in
	// $HERMES_HOME/home/.gitconfig, where the agent's git reads it (S8: Hermes
	// gives the terminal tool HOME=$HERMES_HOME/home under TERMINAL_HOME_MODE=
	// profile).
	if err := useAgentHome(cfg); err != nil {
		return err
	}
	st, err := openStore(cfg.storePath())
	if err != nil {
		return err
	}
	srv := newServer(cfg, bearer, apiKey, st)
	if cfg.Repo != "" {
		co := podagent.NewCheckout(podagent.CheckoutConfig{
			Repo: cfg.Repo, Branch: cfg.RepoBranch, Dir: cfg.Workspace,
			ConnectBase: cfg.ConnectURL, Token: cfg.AgentToken, Voice: hermesdVoice,
		})
		co.Logf = log.Printf
		srv.checkout = co
	}
	srv.sup = newSupervisor(
		func() (gatewayProc, error) {
			return startGateway(cfg.HermesBin, cfg.HermesHome, gatewayEnv(cfg, os.Environ(), apiKey))
		},
		func() error {
			if err := checkNoDotEnv(cfg); err != nil {
				return err
			}
			return writeConfig(cfg)
		},
		srv.api.health, srv.recover, log.Printf,
	)
	if cfg.Repo != "" {
		srv.sup.set(stWaitingCheckout, "")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	hs := &http.Server{Addr: cfg.Listen, Handler: srv.handler(), ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("hermesd: listen %s: %w", cfg.Listen, err)
	}
	log.Printf("hermesd: listening on %s; Hermes API on %s", ln.Addr(), cfg.apiBase())
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()

	if cfg.ConnectURL != "" {
		go podagent.RunGitRoutingLoop(ctx, "hermesd", cfg.ConnectURL, cfg.AgentToken, podagent.GitSyncInterval, log.Printf)
	}
	supDone := make(chan struct{})
	go func() {
		defer close(supDone)
		if srv.checkout != nil {
			go srv.checkout.Run(ctx)
			if !srv.checkout.Wait(ctx) {
				return
			}
		}
		srv.sup.run(ctx)
	}()

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	// SIGTERM: no new turns; the gateway drains (the turns in flight see their
	// runs end) and is waited for, then the listener closes.
	log.Printf("hermesd: shutting down: no new turns; waiting for the gateway to drain")
	srv.shutdown()
	<-supDone
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = hs.Shutdown(shut)
	log.Printf("hermesd: stopped (listener closed: %v)", err)
	return nil
}
