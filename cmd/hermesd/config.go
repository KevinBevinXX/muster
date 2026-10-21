package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/podagent"
)

// config is hermesd's environment, read once.
type config struct {
	HermesHome   string // HERMES_HOME
	StateDir     string // hermesd's own state: the session map and the API key (defaultStateDir; tests set it)
	Workspace    string // terminal.cwd: the checkout when there is a repository
	Instructions string // the instructions file, outside every volume
	Repo         string
	RepoBranch   string
	Model        string
	// ModelBaseURL replaces OpenRouter with an OpenAI-compatible base URL.
	// 🔴 TEST-ONLY: it exists so the image smoke test runs the PRODUCTION image
	// against a stub model. agentspec never sets it (a test pins that), and
	// hermesd logs it at boot whenever it is set.
	ModelBaseURL string
	Listen       string
	APIPort      int
	HermesBin    string // the hermes binary on the image's PATH (tests set another)
	RelayTimeout time.Duration
	ConnectURL   string // muster's connector surface, "" when not offered
	AgentToken   string
}

// approvalsTimeout is the `approvals.timeout` hermesd renders into Hermes's
// config: how long Hermes itself waits for an answer before it denies.
//
// 🔴 hermesd's RELAY TIMEOUT MUST BE SHORTER, AND configFromEnv REFUSES TO START
// OTHERWISE. Measured at the pin (S6): an approval nobody answers is denied by
// Hermes after approvals.timeout, and the run then ends `run.completed`, with a
// tool result starting "BLOCKED: Command timed out without user response" —
// which muster would read as a successful turn. With the relay giving up first,
// the deny is hermesd's own answer, sent while Hermes is still waiting, and the
// turn's outcome is what the agent did after a deny it was told about.
const approvalsTimeout = 300 * time.Second

// defaultRelayTimeout is how long an approval waits for muster's relay.
const defaultRelayTimeout = 240 * time.Second

const (
	defaultHermesHome   = "/data/hermes"
	defaultStateDir     = "/data/hermesd"
	defaultWorkspace    = "/data/workspace"
	defaultInstructions = "/etc/hermesd/instructions.md"
	defaultListen       = ":18789"
	defaultAPIPort      = 8642
)

// hermesdVoice is how the shared checkout names hermesd and the agent.
var hermesdVoice = podagent.Voice{Who: "hermesd", Agent: "the Hermes agent", AgentOf: "its Hermes agent"}

func configFromEnv() (config, error) {
	c := config{
		HermesHome:   envOr("HERMES_HOME", defaultHermesHome),
		StateDir:     defaultStateDir,
		Workspace:    envOr("HERMESD_WORKSPACE", defaultWorkspace),
		Instructions: envOr("HERMESD_INSTRUCTIONS", defaultInstructions),
		Repo:         strings.TrimSpace(os.Getenv("HERMESD_REPO")),
		RepoBranch:   strings.TrimSpace(os.Getenv("HERMESD_REPO_BRANCH")),
		Model:        strings.TrimSpace(os.Getenv("HERMESD_MODEL")),
		ModelBaseURL: strings.TrimSpace(os.Getenv("HERMESD_MODEL_BASE_URL")),
		Listen:       envOr("HERMESD_LISTEN", defaultListen),
		HermesBin:    "hermes",
	}
	var err error
	if c.APIPort, err = envPort("HERMESD_API_PORT", defaultAPIPort); err != nil {
		return c, err
	}
	if c.RelayTimeout, err = envDuration("HERMESD_RELAY_TIMEOUT", defaultRelayTimeout); err != nil {
		return c, err
	}
	if base, tok := strings.TrimRight(os.Getenv(podagent.EnvConnectURL), "/"), os.Getenv(podagent.EnvAgentToken); base != "" && tok != "" {
		c.ConnectURL, c.AgentToken = base, tok
	}
	return c, c.validate()
}

func (c config) validate() error {
	for name, p := range map[string]string{"HERMES_HOME": c.HermesHome, "the state directory": c.StateDir,
		"HERMESD_WORKSPACE": c.Workspace, "HERMESD_INSTRUCTIONS": c.Instructions} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("hermesd: %s=%q must be an absolute path", name, p)
		}
	}
	if c.Repo != "" && !podagent.ParseRepo(c.Repo) {
		return fmt.Errorf("hermesd: HERMESD_REPO=%q is not owner/name", c.Repo)
	}
	if c.RelayTimeout >= approvalsTimeout {
		return fmt.Errorf("hermesd: HERMESD_RELAY_TIMEOUT=%s is not shorter than the rendered approvals.timeout (%s): "+
			"an approval nobody answered would be denied by Hermes itself and the run reported as completed", c.RelayTimeout, approvalsTimeout)
	}
	return nil
}

// The volume layout (docs/design/hermes.md, "Volume layout").
func (c config) agentHome() string  { return filepath.Join(c.HermesHome, "home") }
func (c config) tmpDir() string     { return filepath.Join(c.HermesHome, "cache", "tmp") }
func (c config) configPath() string { return filepath.Join(c.HermesHome, "config.yaml") }
func (c config) envFile() string    { return filepath.Join(c.HermesHome, ".env") }
func (c config) keyPath() string    { return filepath.Join(c.StateDir, "api-server-key") }
func (c config) storePath() string  { return filepath.Join(c.StateDir, "sessions.json") }
func (c config) apiBase() string    { return "http://127.0.0.1:" + strconv.Itoa(c.APIPort) }

// errDotEnv is a $HERMES_HOME/.env on the volume.
//
// 🔴 NO .env ON THE VOLUME, EVER. Hermes loads $HERMES_HOME/.env with
// override=True, so it beats the pod's environment, and it CLEARS known keys it
// does not list (P:hermes_cli/env_loader.py:510-518): a .env would replace or
// remove the provider credential and the API server's settings hermesd hands the
// gateway. hermesd never writes one and never deletes one it did not write: it
// refuses to start the gateway, naming the file — at seed time and again before
// every in-pod restart, because the agent can create one (`hermes config set`).
var errDotEnv = errors.New("a .env exists in HERMES_HOME")

func checkNoDotEnv(c config) error {
	if _, err := os.Lstat(c.envFile()); err == nil {
		return fmt.Errorf("hermesd: %w: %s overrides the pod's environment (provider credential, API server) "+
			"and hermesd refuses to start Hermes with it; remove it", errDotEnv, c.envFile())
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("hermesd: cannot rule out %s: %w", c.envFile(), err)
	}
	return nil
}

// seed creates the volume layout, loads (or once creates) the API key and
// renders Hermes's config. It returns the key. It is run at every start of
// hermesd, and the config is rendered again before every in-pod restart.
func seed(c config) (string, error) {
	for _, d := range []string{c.HermesHome, c.agentHome(), c.tmpDir(), c.StateDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", fmt.Errorf("hermesd: seed: %w", err)
		}
	}
	// The workspace is the checkout when there is a repository; an empty
	// directory is what the checkout clones into, so creating it is harmless.
	if err := os.MkdirAll(c.Workspace, 0o700); err != nil {
		return "", fmt.Errorf("hermesd: seed: %w", err)
	}
	if err := checkNoDotEnv(c); err != nil {
		return "", err
	}
	key, err := loadOrCreateKey(c.keyPath())
	if err != nil {
		return "", err
	}
	if err := writeConfig(c); err != nil {
		return "", err
	}
	return key, nil
}

// loadOrCreateKey returns the API server key kept at path, creating it the
// first time.
//
// 🔴 THE KEY IS GENERATED ONCE AND REUSED BY EVERY START. Hermes scopes runs,
// their idempotency keys and resolve-unknown by a hash of the profile and the
// API key (P:gateway/platforms/api_server_runs.py:375-386), and answers
// GET /v1/runs/{id} and resolve-unknown only to the owner (:1168-1180,
// :1487-1492). A key minted per start would make every recovery call 404 and
// let a same-key re-post admit a second run. It is not derived from the hooks
// token either: muster may rotate that, which would orphan every run.
func loadOrCreateKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		key := strings.TrimSpace(string(b))
		if len(key) < 32 {
			return "", fmt.Errorf("hermesd: the API key file %s is too short to be one hermesd wrote; "+
				"remove it to have a new key generated (every session posted under the old key is then retired)", path)
		}
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0o600 {
			if err := os.Chmod(path, 0o600); err != nil {
				return "", fmt.Errorf("hermesd: the API key file %s: %w", path, err)
			}
		}
		return key, nil
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("hermesd: read the API key: %w", err)
	}
	var rnd [32]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", fmt.Errorf("hermesd: generate the API key: %w", err)
	}
	key := hex.EncodeToString(rnd[:])
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("hermesd: create the API key: %w", err)
	}
	if _, err := f.WriteString(key + "\n"); err != nil {
		f.Close()
		return "", fmt.Errorf("hermesd: write the API key: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", fmt.Errorf("hermesd: write the API key: %w", err)
	}
	return key, f.Close()
}

// keyFingerprint names a key without being one: what the session map records
// for the key a session's runs were posted under.
func keyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// hermesConfig is the rendered config.yaml, as a value (a test reads it back).
//
// It is written as JSON, which is YAML: Hermes reads config.yaml with a YAML
// loader, and JSON keeps hermesd free of a YAML dependency. It is rewritten on
// every start, so nothing the session writes into it (`hermes config set`, a
// /model) survives a restart.
//
//   - `_config_version: 51`: without it every start logs that the config
//     predates auto-migration;
//   - terminal.cwd is the workspace (the checkout): the agent shell's working
//     directory is terminal.cwd, never the gateway's own, and it is what makes
//     the repository's AGENTS.md load (S8, S9);
//   - approvals: manual, with the timeout hermesd's relay is shorter than;
//   - update checks and telemetry off (both explicit);
//   - session history kept (auto_prune off), and no browser, computer-use or
//     voice tools: the pod offers none of them;
//   - no system prompt of any kind: muster's instructions travel on every run
//     (turn.go), and SOUL.md and .hermes.md are never written (the latter would
//     suppress the repository's AGENTS.md, S9).
func hermesConfig(c config) map[string]any {
	cfg := map[string]any{
		"_config_version": 51,
		"terminal":        map[string]any{"backend": "local", "cwd": c.Workspace},
		"approvals":       map[string]any{"mode": "manual", "timeout": int(approvalsTimeout / time.Second)},
		"updates":         map[string]any{"check": false},
		"telemetry":       map[string]any{"shared_metrics": map[string]any{"enabled": false, "send": false}},
		"monitoring": map[string]any{
			"gateway_health_export": map[string]any{"enabled": false},
			"export":                map[string]any{"otlp": map[string]any{"enabled": false}},
		},
		"sessions": map[string]any{"auto_prune": false},
		"agent":    map[string]any{"disabled_toolsets": []string{"browser", "computer_use", "tts"}},
	}
	model := map[string]any{"provider": "openrouter"}
	if c.ModelBaseURL != "" {
		// The key is never written into the file: Hermes expands ${VAR} in its
		// config from the gateway's environment.
		model = map[string]any{"provider": "custom", "base_url": c.ModelBaseURL, "api_key": "${OPENROUTER_API_KEY}"}
	}
	if c.Model != "" {
		model["default"] = c.Model
	}
	cfg["model"] = model
	if c.ConnectURL != "" {
		cfg["mcp_servers"] = map[string]any{"muster": map[string]any{
			"url":     c.ConnectURL + "/mcp",
			"headers": map[string]any{"X-Muster-Token": "${" + podagent.EnvAgentToken + "}"},
		}}
	}
	return cfg
}

func writeConfig(c config) error {
	b, err := json.MarshalIndent(hermesConfig(c), "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(c.configPath(), append(b, '\n'), 0o600)
}

// writeFileAtomic writes path through a temp file in its directory, synced and
// renamed into place, so a reader never sees half a file.
func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	// The rename is durable only once the directory is: a crash after a
	// rename that only reached the page cache could bring the OLD map back,
	// intent and all.
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// gatewayEnv is the environment the gateway is started with: hermesd's own,
// minus what is muster's business, plus the API server's settings.
//
// 🔴 MUSTER'S BEARER AND RAW HOOKS TOKEN NEVER REACH THE GATEWAY, AND SO NEVER
// THE AGENT'S SHELL. Hermes strips provider keys and API_SERVER_* from the
// terminal tool's environment (S8), but not these. MUSTER_HOOK_TOKEN and
// MUSTER_CONNECT_URL DO pass: the agent's git and muster's MCP server use them.
// HERMES_YOLO_MODE is removed too: approvals are never bypassed in a pod.
func gatewayEnv(c config, environ []string, apiKey string) []string {
	drop := map[string]bool{"MUSTER_GATEWAY_BEARER": true, "HOOKS_TOKEN": true, "HERMES_YOLO_MODE": true,
		"HOME": true, "TMPDIR": true, "HERMES_HOME": true, "TERMINAL_HOME_MODE": true, "HERMES_WRITE_SAFE_ROOT": true}
	var env []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if drop[name] || strings.HasPrefix(name, "API_SERVER_") || strings.HasPrefix(name, "HERMESD_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"HERMES_HOME="+c.HermesHome,
		// The agent shell's HOME is $HERMES_HOME/home only when that directory
		// exists AND the mode is profile (or a container is detected,
		// P:hermes_constants.py:743-768); seed creates it, and the mode is set
		// rather than left to detection.
		"HOME="+c.agentHome(),
		"TERMINAL_HOME_MODE=profile",
		"TMPDIR="+c.tmpDir(),
		"HERMES_WRITE_SAFE_ROOT="+filepath.Dir(c.HermesHome),
		"API_SERVER_ENABLED=true",
		"API_SERVER_HOST=127.0.0.1",
		"API_SERVER_PORT="+strconv.Itoa(c.APIPort),
		"API_SERVER_KEY="+apiKey,
	)
}
