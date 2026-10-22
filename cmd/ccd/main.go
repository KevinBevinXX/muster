// Command ccd is the in-pod supervisor that makes an interactive Claude Code
// session a muster agent runtime.
//
// One `claude` TUI runs in a tmux session; it is the conversation, and the one an
// operator sees when they attach. ccd speaks muster's existing agent wire on top
// of it:
//
//   - POST /v1/responses — muster's chat/kickoff transport. The prompt goes IN as
//     a bracketed paste into the pane (tmux.go); the reply comes OUT of the
//     session transcript JSONL (transcript.go), never off the screen; the turn
//     ends on the CLI's Stop / StopFailure hook (server.go). A failed turn is a
//     non-200 with a typed JSON error (failure.go), never a 200 with no text.
//   - GET /healthz (and GET /, identically) — 200 while ccd is up, the tmux
//     session (once the CLI is started: the CLI's own pane, on the tmux server it
//     was started in) is alive, and the CLI is neither crash-looping nor lost.
//     Session and auth state are reported in the body and never gate it
//     (server.go handleHealthz).
//   - /hook/{event} on a UNIX SOCKET in a directory only ccd's user can enter,
//     authenticated with a per-process credential — where `ccd hook <Event>`, the
//     command the CLI's settings run for each hook, reports (server.go
//     hookHandler). ccd hands the socket's path and the credential to each CLI it
//     starts, in its environment (CCD_HOOK_SOCKET, CCD_HOOK_SECRET).
//
// Subcommands:
//
//	ccd [serve]          run the server (environment below)
//	ccd hook <Event>     forward a hook payload from stdin to the server
//	ccd seed [--check]   write (or verify) the CLI's onboarding/trust/hook config
//	ccd approve          the PermissionRequest hook: route the prompt to the
//	                     operator through muster's approval relay (approve.go)
//
// Environment (serve):
//
//	MUSTER_GATEWAY_BEARER  the bearer /v1/responses accepts (muster mints it per agent)
//	HOOKS_TOKEN            alternatively, a token it is derived from (both: must agree)
//	CCD_GATEWAY_UPSTREAM_BEARER  the gateway runtime's OWN credential, which ccd
//	                       presents when it hands a gateway turn on (never its own)
//	                       🔴 ccd unsets all three before it starts anything, and
//	                       refuses a bearer the CLI could derive from what it keeps
//	                       (MUSTER_HOOK_TOKEN): turncred.go
//	CLAUDE_CONFIG_DIR      the CLI's config dir; transcripts are read only under <it>/projects
//	CCD_LISTEN             gateway address        (default :18789)
//	                       (the hook listener is a unix socket ccd creates itself under
//	                       $TMPDIR; CCD_HOOK_LISTEN, the old TCP address, is no longer read)
//	CCD_TMUX_SOCKET        tmux -L socket name    (default: tmux's default server)
//	CCD_TMUX_CONF          tmux -f config file    (default: tmux's default)
//	CCD_TMUX_TARGET        tmux SESSION name      (default cc); never an input target:
//	                       prompts go to the CLI's pane id (tmux.go)
//	CCD_WORKSPACE          the CLI's working directory (default: cwd)
//	CCD_REPO               owner/name: clone https://github.com/<it> into CCD_WORKSPACE before the
//	                       CLI starts, and start the CLI only once it exists (checkout.go)
//	CCD_REPO_BRANCH        the branch to clone (default: the remote's default)
//	CCD_SUPERVISE          "1": once listening, create the tmux session and keep the
//	                       CLI running in it — `claude --resume <id>` when a transcript
//	                       exists, `claude` on a fresh volume (supervise.go). Without
//	                       it ccd knows no pane id, so every turn is not_ready.
//	CCD_CLAUDE_BIN         the CLI binary         (default claude)
//	CCD_GATEWAY_UPSTREAM   the gateway runtime beside ccd, http://127.0.0.1:<port> (loopback
//	                       only): a request with X-Muster-Runtime: gateway is handed to it
//	                       (gateway.go). Unset: such a request is not_ready
//	CCD_MAX_SESSIONS       how many CLIs ccd runs BESIDE the primary, one per explicit
//	                       muster session (lanes.go; default 3, 0 = none: a request
//	                       naming a session is then not_ready)
//	CCD_SUBMIT_TIMEOUT / CCD_TURN_TIMEOUT              (default 20s / 30m)
//	CCD_HOOK_LOG           append every raw hook payload to this file. OFF by default;
//	                       when on it writes every prompt's text there (on the volume,
//	                       when pointed under /data). Capture-only, for the OWED note in
//	                       failure.go, and removed by the PR that closes it.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ZacxDev/muster/internal/cctasks"
)

const defaultListen = ":18789"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		if err := serve(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case "hook":
		return runHook(args, os.Stdin, os.Stderr)
	case "seed":
		return runSeed(args, os.Stdout, os.Stderr)
	case "connect-git":
		return runConnectGit(args, os.Stdout, os.Stderr)
	case "approve":
		return runApprove(args, os.Stdin, os.Stdout, os.Stderr)
	default:
		fmt.Fprintf(os.Stderr, "ccd: unknown command %q (serve | hook <Event> | seed | connect-git [connector] | approve)\n", cmd)
		return 2
	}
}

// defaultTmuxTarget is the tmux SESSION ccd runs the CLI in when
// CCD_TMUX_TARGET is unset. muster's terminal attaches to it by the same
// variable and default (internal/agents.TerminalCommand); TestTheTerminalAttachesToCcdsSession pins
// the two together.
const defaultTmuxTarget = "cc"

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// envCount reads a non-negative integer, def when unset.
func envCount(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("ccd: %s=%q is not a non-negative integer", name, v)
	}
	return n, nil
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("ccd: %s=%q is not a positive duration", name, v)
	}
	return d, nil
}

// hookChannel is ccd's hook listener and the credential it accepts.
//
// 🔴 A UNIX SOCKET, NOT 127.0.0.1. Every container in a pod shares one network
// namespace, so a TCP loopback listener is reachable from a sidecar — in a
// chief's pod, the gateway runtime, which untrusted content can steer. A socket
// bound to a path is reachable only through the filesystem, and the sidecar has
// its own (agentspec's gateway sidecar mounts no volume of the chief's). The
// directory is os.MkdirTemp's, mode 0700, owned by ccd's user. The credential is
// per process: 32 random bytes, never written to disk, handed only to the CLIs
// ccd starts (tmuxTerminal.env). It is defence in depth — it keeps the channel
// authenticated if the socket's isolation ever fails (a shared volume, a shared
// process namespace) — and it does NOT stop a process of ccd's own user, every
// tool the CLI runs included, which inherits it (server.go hookHandler).
type hookChannel struct {
	dir, path, secret string
}

func newHookChannel() (*hookChannel, error) {
	var rnd [32]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, fmt.Errorf("ccd: hook credential: %w", err)
	}
	// MkdirTemp creates it mode 0700 (TestTheHookChannelIsAPrivateSocketWithAPerProcessCredential).
	dir, err := os.MkdirTemp("", "ccd-hook-")
	if err != nil {
		return nil, fmt.Errorf("ccd: hook socket directory: %w", err)
	}
	return &hookChannel{dir: dir, path: filepath.Join(dir, "hook.sock"), secret: hex.EncodeToString(rnd[:])}, nil
}

// listen binds the socket, readable and writable by ccd's user only.
func (h *hookChannel) listen() (net.Listener, error) {
	ln, err := net.Listen("unix", h.path)
	if err != nil {
		return nil, fmt.Errorf("ccd: listen %s: %w", h.path, err)
	}
	if err := os.Chmod(h.path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("ccd: hook socket: %w", err)
	}
	return ln, nil
}

// cliEnv is what every CLI ccd starts gets in its environment.
func (h *hookChannel) cliEnv() []string {
	return []string{envHookSocket + "=" + h.path, envHookSecret + "=" + h.secret}
}

func serve() error {
	// 🔴 FIRST, BEFORE ccd STARTS ANY PROCESS: take the turn-API credentials out of
	// the environment every child inherits, and make ccd's own /proc unreadable to
	// its user (turncred.go). The CLI and its tool calls then hold nothing that
	// drives ccd's turn API or the gateway runtime's.
	creds := takeTurnCredentials()
	if err := hideFromSameUser(); err != nil {
		return err
	}
	bearer, err := resolveBearer(creds.hooksToken, creds.gatewayBearer)
	if err != nil {
		return err
	}
	if name := exposedTurnCredential(bearer, os.Getenv); name != "" {
		// Names only, never a value.
		return fmt.Errorf("ccd: the turn credential is derivable from %s, which the CLI keeps in its environment, "+
			"so any tool call could drive this turn API; muster must ship %s as a credential of its own "+
			"(this pod's spec predates it: Stop → Start the agent)", name, envGatewayBearer)
	}
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if configDir == "" {
		home, _ := os.UserHomeDir()
		configDir = filepath.Join(home, ".claude")
	}
	hooks, err := newHookChannel()
	if err != nil {
		return err
	}
	defer os.RemoveAll(hooks.dir)
	var durs [2]time.Duration
	for i, d := range []struct {
		name string
		def  time.Duration
	}{{"CCD_SUBMIT_TIMEOUT", 20 * time.Second}, {"CCD_TURN_TIMEOUT", 30 * time.Minute}} {
		if durs[i], err = envDuration(d.name, d.def); err != nil {
			return err
		}
	}
	workspace := os.Getenv("CCD_WORKSPACE")
	if workspace == "" {
		workspace, _ = os.Getwd()
	}

	co, err := checkoutFromEnv(workspace)
	if err != nil {
		return err
	}

	term := tmuxTerminal{
		bin: "tmux", socket: os.Getenv("CCD_TMUX_SOCKET"), conf: os.Getenv("CCD_TMUX_CONF"),
		target: envOr("CCD_TMUX_TARGET", defaultTmuxTarget), enterDelay: 150 * time.Millisecond,
		env: hooks.cliEnv(),
	}
	auth := newAuthTracker()
	srv := newServer(serverConfig{
		Bearer: bearer, HookSecret: hooks.secret, ConfigDir: configDir,
		SubmitTimeout: durs[0], TurnTimeout: durs[1], HookLog: os.Getenv("CCD_HOOK_LOG"),
		HasCredential: credentialVisible(configDir),
	}, term, auth)
	// The "awaiting approval" badge (blocked.go): set before the hook listener
	// serves, so onHook never races its assignment.
	if base, tok := strings.TrimRight(os.Getenv(envConnectURL), "/"), os.Getenv(envAgentToken); base != "" && tok != "" {
		srv.blocked = newBlockedReporter(base, tok)
		// The live work counts on the agent's card (work.go), on the same token.
		counter := &podCounter{configDir: configDir, root: procRoot}
		srv.work = newWorkReporter(base, tok, func() cctasks.Work {
			w, _ := counter.work(context.Background(), srv.workServers())
			return w
		}, log.Printf)
	}
	if up := os.Getenv("CCD_GATEWAY_UPSTREAM"); up != "" {
		// A missing or reused runtime credential refuses gateway turns only;
		// failing the process would take the Claude Code sessions down with it.
		gwBearer := creds.upstreamBearer
		if why := gatewayBearerProblem(gwBearer, bearer); why != "" {
			log.Printf("ccd: gateway turns refused: %s", why)
			srv.gatewayOff = why
		} else if srv.gateway, err = newGatewayProxy(up, gwBearer); err != nil {
			return err
		}
	}
	srv.checkout = co
	if os.Getenv("CCD_SUPERVISE") == "1" {
		bin := envOr("CCD_CLAUDE_BIN", "claude")
		srv.supervisePrimary(newSupervisor(term, bin, configDir, workspace), term.PanePID)
		maxLanes, err := envCount("CCD_MAX_SESSIONS", defaultMaxLanes)
		if err != nil {
			return err
		}
		if maxLanes > 0 {
			srv.runLanes(&lanes{
				newTerm: func(target string) laneTerminal {
					t := term
					t.target = target
					return t
				},
				newServer: func(t terminal) *server {
					ls := newServer(serverConfig{Bearer: bearer, ConfigDir: configDir,
						SubmitTimeout: durs[0], TurnTimeout: durs[1], HookLog: os.Getenv("CCD_HOOK_LOG"),
						HasCredential: credentialVisible(configDir)}, t, auth)
					// The agent's one badge: a lane's prompt shows on it too.
					ls.blocked = srv.blocked
					return ls
				},
				bin: bin, configDir: configDir, workspace: workspace, max: maxLanes,
				startTimeout: laneStartTimeout, now: time.Now,
				set: map[string]*lane{},
			})
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	gw := &http.Server{Addr: envOr("CCD_LISTEN", defaultListen), Handler: srv.gatewayHandler(),
		ReadHeaderTimeout: 10 * time.Second}
	hk := &http.Server{Handler: srv.hookHandler(), ReadHeaderTimeout: 5 * time.Second}

	errc := make(chan error, 2)
	gln, err := net.Listen("tcp", gw.Addr)
	if err != nil {
		return fmt.Errorf("ccd: listen %s: %w", gw.Addr, err)
	}
	hln, err := hooks.listen()
	if err != nil {
		gln.Close()
		return err
	}
	for _, l := range []struct {
		s  *http.Server
		ln net.Listener
	}{{gw, gln}, {hk, hln}} {
		log.Printf("ccd: listening on %s", l.ln.Addr())
		go func(s *http.Server, ln net.Listener) { errc <- s.Serve(ln) }(l.s, l.ln)
	}
	// 🔴 THE SESSION STARTS ONLY ONCE BOTH LISTENERS ARE BOUND. Its SessionStart
	// hook, sent as the CLI starts, is ccd's one readiness signal for turns; a TUI
	// started before the hook listener exists would announce itself to nobody, and
	// ccd would answer not_ready until the CLI next restarted.
	// muster's connector store: keep git's routing in step with this agent's
	// grants for as long as ccd runs (connect.go). Inert when the pod is not
	// offered the store.
	if base, tok := strings.TrimRight(os.Getenv(envConnectURL), "/"), os.Getenv(envAgentToken); base != "" && tok != "" {
		go runGitRoutingLoop(ctx, base, tok, gitSyncInterval, log.Printf)
	}
	if srv.blocked != nil {
		go srv.blocked.run(ctx)
	}
	if srv.work != nil {
		go srv.work.run(ctx)
	}
	// The agent's repository is checked out before the CLI starts, and the CLI
	// starts in it (checkout.go). No repository: the CLI starts at once, as
	// before.
	if co != nil {
		go co.Run(ctx)
	}
	var supErr chan error // nil (never ready) unless ccd supervises the CLI
	if srv.sup != nil {
		supErr = make(chan error, 1)
		go func() {
			if co != nil && !co.Wait(ctx) {
				supErr <- nil // shutting down before the checkout existed
				return
			}
			supErr <- srv.sup.run(ctx)
		}()
	}
	for done := false; !done; {
		select {
		case <-ctx.Done():
			done = true
		case err := <-supErr:
			// nil: crash_loop or terminal_lost (or shutdown). ccd keeps serving
			// /healthz, which now fails, so Kubernetes rather than ccd restarts the
			// pod. An error is a CLI that could not be started at all.
			if err != nil {
				return fmt.Errorf("ccd: start the session: %w", err)
			}
			supErr = nil
		case err := <-errc:
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			done = true
		}
	}
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = gw.Shutdown(shut)
	_ = hk.Shutdown(shut)
	return nil
}
