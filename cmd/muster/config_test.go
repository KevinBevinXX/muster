package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "muster.env")
	content := `# muster machine-client config
# a comment with an = sign in it

MUSTER_API_URL=http://127.0.0.1:8080
export MUSTER_HOOK_TOKEN="quoted-token"
SINGLE='single-quoted'
  SPACED   =   value with spaces
NOEQUALS
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := parseEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"MUSTER_API_URL":    "http://127.0.0.1:8080",
		"MUSTER_HOOK_TOKEN": "quoted-token",
		"SINGLE":            "single-quoted",
		"SPACED":            "value with spaces",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("parseEnvFile()[%q] = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["NOEQUALS"]; ok {
		t.Errorf("a line without '=' produced a key")
	}
	if len(got) != len(want) {
		t.Errorf("parsed %v, want exactly %v", got, want)
	}
}

func TestParseEnvFileMissingIsNotAnError(t *testing.T) {
	got, err := parseEnvFile(filepath.Join(t.TempDir(), "absent.env"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

// TestTheEnvVarNamesAreLiteral pins the two variable names against LITERAL
// strings.
//
// 🔴 THE LITERALS ARE THE WHOLE POINT, for the reason
// TestSessionIDEnvVarNamesAreLiteral gives at length: a test that keys its fake
// environment on the production constant resolves whatever that constant says,
// so the constant's VALUE is never under test. That is how a whole feature
// shipped inert upstream behind a green suite. These two names are also the
// entire contract between this binary and however a host provisions it — a
// rename here is a silent "no API URL" on every machine that was already
// configured.
func TestTheEnvVarNamesAreLiteral(t *testing.T) {
	if envAPIURL != "MUSTER_API_URL" {
		t.Errorf("envAPIURL = %q, want the literal %q", envAPIURL, "MUSTER_API_URL")
	}
	if envToken != "MUSTER_TOKEN" {
		t.Errorf("envToken = %q, want the literal %q", envToken, "MUSTER_TOKEN")
	}
	if envTokenAlias != "MUSTER_HOOK_TOKEN" {
		t.Errorf("envTokenAlias = %q, want the literal %q", envTokenAlias, "MUSTER_HOOK_TOKEN")
	}
	// Behavioural half, over a literal map: the names are actually READ, not
	// merely declared.
	c, err := resolveConfig("f", nil, func(k string) string {
		return map[string]string{
			"MUSTER_API_URL":    "http://from-literal:9",
			"MUSTER_HOOK_TOKEN": "literal-token",
		}[k]
	}, "", "")
	if err != nil {
		t.Fatalf("resolveConfig with literally-named env vars failed: %v", err)
	}
	if c.apiURL != "http://from-literal:9" || c.token != "literal-token" {
		t.Fatalf("resolved %+v from literally-named variables", c)
	}
}

// TestTheTokenAliasIsReadAndLosesToTheNewName: MUSTER_HOOK_TOKEN alone still
// configures the client (every hook host and agent pod spells it so);
// MUSTER_TOKEN beats it within a source; a higher source's alias still beats a
// lower source's MUSTER_TOKEN. Literal names, for the reason above.
func TestTheTokenAliasIsReadAndLosesToTheNewName(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		name string
		file map[string]string
		env  map[string]string
		want string
	}{
		{"alias only (env)", nil, map[string]string{"MUSTER_HOOK_TOKEN": "hook"}, "hook"},
		{"alias only (file)", map[string]string{"MUSTER_HOOK_TOKEN": "hook"}, nil, "hook"},
		{"both in env", nil, map[string]string{"MUSTER_HOOK_TOKEN": "hook", "MUSTER_TOKEN": "personal"}, "personal"},
		{"both in file", map[string]string{"MUSTER_TOKEN": "personal", "MUSTER_HOOK_TOKEN": "hook"}, nil, "personal"},
		{"env alias beats file name", map[string]string{"MUSTER_TOKEN": "file"}, map[string]string{"MUSTER_HOOK_TOKEN": "env"}, "env"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := map[string]string{"MUSTER_API_URL": "http://x:1"}
			for k, v := range c.env {
				e[k] = v
			}
			got, err := resolveConfig("f", c.file, env(e), "", "")
			if err != nil || got.token != c.want {
				t.Fatalf("token = %q (%v); want %q", got.token, err, c.want)
			}
		})
	}
}

func TestResolveConfigPrecedence(t *testing.T) {
	file := map[string]string{envAPIURL: "http://from-file:1", envToken: "file-token"}
	env := map[string]string{envAPIURL: "http://from-env:2", envToken: "env-token"}
	getenv := func(k string) string { return env[k] }

	t.Run("file only", func(t *testing.T) {
		c, err := resolveConfig("f", file, func(string) string { return "" }, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if c.apiURL != "http://from-file:1" || c.token != "file-token" {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("environment beats file", func(t *testing.T) {
		c, err := resolveConfig("f", file, getenv, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if c.apiURL != "http://from-env:2" || c.token != "env-token" {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("flags beat everything", func(t *testing.T) {
		c, err := resolveConfig("f", file, getenv, "http://from-flag:3", "flag-token")
		if err != nil {
			t.Fatal(err)
		}
		if c.apiURL != "http://from-flag:3" || c.token != "flag-token" {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("an empty higher source does not blank a lower one", func(t *testing.T) {
		c, err := resolveConfig("f", file, func(string) string { return "" }, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if c.token != "file-token" {
			t.Fatalf("token = %q, want the file value preserved", c.token)
		}
	})
	// 🔴 AN UNRECOGNISED KEY IS IGNORED, NOT ADOPTED BY A NEIGHBOUR. A host's
	// env file is shared with whatever else that host runs, so it will carry keys
	// this binary has never heard of — including credentials for a DIFFERENT
	// service. Adopting one would present the wrong secret and produce a 401 whose
	// message names the wrong variable to rotate.
	t.Run("an unknown key is ignored rather than adopted", func(t *testing.T) {
		const foreign = "SOME_OTHER_SERVICE_TOKEN"
		const leak = "a-secret-belonging-to-something-else"
		file := map[string]string{envAPIURL: "http://h:1", foreign: leak}
		c, err := resolveConfig("f", file, func(k string) string {
			return map[string]string{foreign: leak}[k]
		}, "", "")
		if err != nil {
			t.Fatalf("a foreign key made config resolution fail: %v", err)
		}
		if c.token == leak {
			t.Errorf("🔴 %s was adopted as this binary's credential: %+v", foreign, c)
		}
		// Non-vacuity: the call really did resolve something, so a wiring that
		// silently returned a zero config could not pass the assertion above.
		if c.apiURL != "http://h:1" {
			t.Fatalf("apiURL = %q — the fixture did not resolve, so the assertion above is vacuous", c.apiURL)
		}
	})
	t.Run("trailing slash trimmed", func(t *testing.T) {
		c, err := resolveConfig("f", nil, func(string) string { return "" }, "http://h:1/", "")
		if err != nil {
			t.Fatal(err)
		}
		if c.apiURL != "http://h:1" {
			t.Fatalf("apiURL = %q", c.apiURL)
		}
	})
}

func TestResolveConfigErrors(t *testing.T) {
	none := func(string) string { return "" }
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"missing", "", "no API URL"},
		{"relative", "127.0.0.1:8080", "invalid API URL"},
		{"bad scheme", "ftp://h/x", "invalid API URL scheme"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveConfig(defaultEnvFile, nil, none, tc.url, "")
			if err == nil {
				t.Fatalf("want an error for %q", tc.url)
			}
			if exitCodeFor(err) != exitUsage {
				t.Fatalf("exit = %d, want %d", exitCodeFor(err), exitUsage)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestResolveConfigNeverEchoesToken: a bad-URL error is the message most likely
// to dump "everything we resolved".
func TestResolveConfigNeverEchoesToken(t *testing.T) {
	_, err := resolveConfig("f", map[string]string{envToken: "super-secret"}, func(string) string { return "" }, "", "")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("token leaked into the error: %q", err)
	}
}

func TestExpandHome(t *testing.T) {
	// ⚠ THE FIXTURE HOME IS NOT UNDER /home/<somebody>, AND THAT IS THE LEAK
	// GATE RATHER THAN TASTE: tests/leakscan.py refuses an absolute path under a
	// home directory because it names a person and is not reproducible elsewhere.
	const fixtureHome = "/example/operator-home"
	cases := []struct{ in, homeDir, want string }{
		{"~/.muster/muster.env", fixtureHome, fixtureHome + "/.muster/muster.env"},
		{"~", fixtureHome, fixtureHome},
		{"/abs/path", fixtureHome, "/abs/path"},
		{"relative/path", fixtureHome, "relative/path"},
		{"~/x", "", "~/x"},
		// A "~" that is not a PREFIX is an ordinary path character.
		{"/abs/~/path", fixtureHome, "/abs/~/path"},
	}
	for _, tc := range cases {
		if got := expandHome(tc.in, tc.homeDir); got != tc.want {
			t.Errorf("expandHome(%q, %q) = %q, want %q", tc.in, tc.homeDir, got, tc.want)
		}
	}
}

// TestTheVerbsWorkFromTheEnvironmentAlone is the dispatched-instance control.
//
// 🔴 EVERY OTHER TEST HERE PASSES --api-url AND --token ON THE COMMAND LINE,
// which is exactly the configuration a dispatched agent instance does NOT have:
// it carries the two variables in its environment and nothing else. A "the
// agent verbs work from inside an instance" claim made with flags would be
// measuring the operator shell a second time.
func TestTheVerbsWorkFromTheEnvironmentAlone(t *testing.T) {
	h := newHarness(t)
	h.json("GET /agent/task", http.StatusOK, `{"id":42,"status":"in_progress"}`)

	got := h.runCLINoCredFlags(map[string]string{
		envAPIURL: h.srv.URL,
		envToken:  testToken,
	}, filepath.Join(t.TempDir(), "absent.env"), "agent", "task", "get")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q — the verbs must work with no flags at all", got.code, got.stderr)
	}
	reqs := h.nonHealthRequests()
	if len(reqs) != 1 || reqs[0].auth != "Bearer "+testToken {
		t.Fatalf("requests = %+v, want one carrying the bearer token from the environment", reqs)
	}
}

// TestTheVerbsWorkFromTheEnvFileAlone is the same control one source lower: the
// file is what a provisioner writes, and a file whose keys nothing reads is the
// silent half of a misconfiguration.
func TestTheVerbsWorkFromTheEnvFileAlone(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/tasks", http.StatusOK, `[]`)

	path := filepath.Join(t.TempDir(), "muster.env")
	body := envAPIURL + "=" + h.srv.URL + "\nexport " + envToken + "=\"" + testToken + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got := h.runCLINoCredFlags(nil, path, "task", "ls")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	reqs := h.nonHealthRequests()
	if len(reqs) != 1 || reqs[0].auth != "Bearer "+testToken {
		t.Fatalf("requests = %+v, want one carrying the bearer token from the env FILE", reqs)
	}
}
