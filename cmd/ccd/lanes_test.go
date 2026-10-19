package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestTheLaneHeadersAreMustersHeaders: the request headers ccd routes a lane
// turn by are the ones muster's transport sets — two binaries, one contract.
func TestTheLaneHeadersAreMustersHeaders(t *testing.T) {
	if headerCCSession != agents.HeaderCCSession || headerCCModel != agents.HeaderCCModel {
		t.Fatalf("ccd reads %q/%q, muster sends %q/%q", headerCCSession, headerCCModel, agents.HeaderCCSession, agents.HeaderCCModel)
	}
}

// TestTheLaneModelSetIsMusters: every model muster accepts for a claude-code
// session, ccd passes to --model, and ccd refuses what muster refuses — so a
// session muster created can always start, and nothing muster would not store
// reaches argv.
func TestTheLaneModelSetIsMusters(t *testing.T) {
	for _, m := range []string{"opus", "sonnet", "haiku", "claude-opus-4-1", "claude-sonnet-4-5[1m]", "x.y-z"} {
		if _, err := agents.ValidateSessionRuntime(agents.SessionRuntimeClaudeCode, m); err != nil || !laneModelPattern.MatchString(m) {
			t.Errorf("%q: muster %v, ccd %v; want both to accept", m, err, laneModelPattern.MatchString(m))
		}
	}
	for _, m := range []string{"Opus", "-opus", "opus --x", "opus;rm", "op us", "opus[2m]", strings.Repeat("a", 81)} {
		_, err := agents.ValidateSessionRuntime(agents.SessionRuntimeClaudeCode, m)
		if err == nil || laneModelPattern.MatchString(m) {
			t.Errorf("%q: muster %v, ccd accepts=%v; want both to refuse", m, err, laneModelPattern.MatchString(m))
		}
	}
	// muster trims what it is given before it stores it; what it STORES (and so
	// sends) is what ccd must accept.
	if stored, err := agents.ValidateSessionRuntime(agents.SessionRuntimeClaudeCode, " opus\n"); err != nil || !laneModelPattern.MatchString(stored) || laneModelPattern.MatchString(" opus\n") {
		t.Errorf("a padded model: muster stores %q (%v), ccd accepts it=%v, accepts the padded one=%v", stored, err,
			laneModelPattern.MatchString(stored), laneModelPattern.MatchString(" opus\n"))
	}
}

// TestLaneIDsAndUUIDs: a lane id is 12 hex; a lane's session id is
// deterministic per key, distinct across keys, carries the marker, and passes
// the STRICT UUID check the pinned CLI applies somewhere (version 1-5, variant
// 8-b) as well as the loose one.
func TestLaneIDsAndUUIDs(t *testing.T) {
	strict := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for _, k := range []string{"agent:chief:webchat:1", "agent:chief:webchat:2", "", "x y\nz"} {
		id, u := laneID(k), laneUUID(k)
		if !isLaneID(id) || laneID(k) != id || laneUUID(k) != u {
			t.Errorf("%q: id %q uuid %q not stable or not hex", k, id, u)
		}
		if !strict.MatchString(u) || !sessionIDPattern.MatchString(u) || !strings.HasPrefix(u, laneUUIDMarker+"-") {
			t.Errorf("%q: uuid %q fails the CLI's checks or lacks the marker", k, u)
		}
		if seen[u] || seen[id] {
			t.Errorf("%q: collides", k)
		}
		seen[u], seen[id] = true, true
	}
	for _, bad := range []string{"", "ABCDEF012345", "abc", "abcdef0123456", "abcdef01234g", "../../etc/x"} {
		if isLaneID(bad) {
			t.Errorf("isLaneID(%q)", bad)
		}
	}
}

// TestThePrimaryRecordNeverNamesALane: a lane's id is never recorded as the
// primary's, a malformed one is refused, and a record whose transcript is gone
// resolves to nothing.
func TestThePrimaryRecordNeverNamesALane(t *testing.T) {
	cfg, ws := t.TempDir(), "/data/workspace"
	if err := recordPrimary(cfg, laneUUID("k")); err == nil {
		t.Error("a lane's session id was recorded as the primary's")
	}
	if err := recordPrimary(cfg, "not-an-id; rm -rf /"); err == nil {
		t.Error("a malformed id was recorded")
	}
	id := "0f0e0d0c-0b0a-4908-8706-050403020100"
	if err := recordPrimary(cfg, id); err != nil {
		t.Fatal(err)
	}
	if got := recordedPrimary(cfg, ws); got != "" {
		t.Errorf("a record with no transcript resolved to %q", got)
	}
	dir := projectDir(cfg, ws)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := recordedPrimary(cfg, ws); got != id {
		t.Errorf("recordedPrimary = %q, want %q", got, id)
	}
}

// TestThePrimaryFallbackNeverResumesALane: with no record, a directory holding a
// lane transcript NEWER than the primary's resumes the primary's by id (not
// --continue, which would take the newest), and one holding only lane
// transcripts starts fresh; with no lane transcript it is still by id — the
// NEWEST non-lane one, never a bare --continue.
func TestThePrimaryFallbackNeverResumesALane(t *testing.T) {
	cfg, ws := t.TempDir(), "/data/workspace"
	dir := projectDir(cfg, ws)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sup := newSupervisor(nil, "claude", cfg, ws)
	write := func(id string, at time.Time) {
		p := filepath.Join(dir, id+".jsonl")
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	lane := laneUUID("k")
	write(lane, now)
	if argv, mode := sup.argv(); strings.Join(argv, " ") != "claude" || mode != modeFresh {
		t.Errorf("only a lane's transcript: %v %s; want a fresh start", argv, mode)
	}
	primary := "0f0e0d0c-0b0a-4908-8706-050403020100"
	write(primary, now.Add(-time.Hour))
	if argv, mode := sup.argv(); strings.Join(argv, " ") != "claude --resume "+primary || mode != modeContinue {
		t.Errorf("a newer lane transcript beside the primary's: %v %s; want --resume %s", argv, mode, primary)
	}
	if err := os.Remove(filepath.Join(dir, lane+".jsonl")); err != nil {
		t.Fatal(err)
	}
	if argv, _ := sup.argv(); strings.Join(argv, " ") != "claude --resume "+primary {
		t.Errorf("no lane transcript: %v; want --resume %s", argv, primary)
	}
	// Two non-lane transcripts: the newer is resumed, whatever the names' order.
	older := "ffffffff-0b0a-4908-8706-050403020100"
	write(older, now.Add(-2*time.Hour))
	if argv, _ := sup.argv(); strings.Join(argv, " ") != "claude --resume "+primary {
		t.Errorf("an older non-lane transcript beside the primary's: %v; want --resume %s (the newest)", argv, primary)
	}
	// A record beats "the newest": the recorded conversation is resumed though
	// another non-lane one is newer.
	if err := recordPrimary(cfg, older); err != nil {
		t.Fatal(err)
	}
	if argv, _ := sup.argv(); strings.Join(argv, " ") != "claude --resume "+older {
		t.Errorf("recorded %s, newer %s beside it: %v; want --resume %s (the record)", older, primary, argv, older)
	}
}

// TestALaneHookReachesItsLaneAndNoOther: a hook carrying ?lane= updates that
// lane's session and not the primary's; one for a lane that is not running is
// dropped; one with no lane is the primary's, and the primary's SessionStart is
// recorded.
func TestALaneHookReachesItsLaneAndNoOther(t *testing.T) {
	cfg := t.TempDir()
	old := procRoot
	procRoot = fakeProc(t, chiefTree)
	t.Cleanup(func() { procRoot = old })
	primary := newServer(serverConfig{ConfigDir: cfg}, nil, newAuthTracker())
	primary.recordsPrimary = true
	primary.primaryPID = func(context.Context) (int, error) { return 20, nil }
	laneSrv := newServer(serverConfig{ConfigDir: cfg}, nil, newAuthTracker())
	id := laneID("k")
	primary.lanes = &lanes{set: map[string]*lane{id: {id: id, key: "k", srv: laneSrv}}}
	hs := httptest.NewServer(primary.hookHandler())
	t.Cleanup(hs.Close)
	post := func(q, body string) {
		resp, err := http.Post(hs.URL+"/hook/SessionStart"+q, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// A lane's SessionStart with an id that is NOT marked as a lane's — what a
	// /clear in the lane's CLI starts — is still the lane's, and is not recorded.
	post("?lane="+id, `{"hook_event_name":"SessionStart","session_id":"11111111-2222-4333-8444-555555555555"}`)
	if !laneSrv.started() || primary.started() {
		t.Fatalf("a lane's SessionStart: lane started=%v primary started=%v", laneSrv.started(), primary.started())
	}
	if _, err := os.Stat(filepath.Join(cfg, primaryRecordFile)); err == nil {
		t.Fatal("a lane's SessionStart was recorded as the primary's")
	}
	post("?lane=aaaaaaaaaaaa", `{"hook_event_name":"SessionStart","session_id":"0f0e0d0c-0b0a-4908-8706-050403020100"}`)
	if primary.started() {
		t.Fatal("a hook for a lane that is not running reached the primary")
	}
	post("?pid=31", `{"hook_event_name":"SessionStart","session_id":"0f0e0d0c-0b0a-4908-8706-050403020100"}`)
	if !primary.started() {
		t.Fatal("a hook with no lane did not reach the primary")
	}
	if b, err := os.ReadFile(filepath.Join(cfg, primaryRecordFile)); err != nil || strings.TrimSpace(string(b)) != "0f0e0d0c-0b0a-4908-8706-050403020100" {
		t.Fatalf("the primary's SessionStart was not recorded: %q %v", b, err)
	}
}

// TestRunHookSendsItsLane: `ccd hook` in a lane's CLI (CCD_LANE set) sends the
// lane; a malformed CCD_LANE is not put in the URL.
func TestRunHookSendsItsLane(t *testing.T) {
	var got []string
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.RequestURI())
	}))
	t.Cleanup(hs.Close)
	t.Setenv("CCD_HOOK_URL", hs.URL)
	for _, lane := range []string{laneID("k"), "../x?y=z", ""} {
		t.Setenv("CCD_LANE", lane)
		runHook([]string{"Stop"}, strings.NewReader(`{}`), io.Discard)
	}
	pid := strconv.Itoa(os.Getpid())
	want := []string{"/hook/Stop?pid=" + pid + "&lane=" + laneID("k"), "/hook/Stop?pid=" + pid, "/hook/Stop?pid=" + pid}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("hook URLs %v, want %v", got, want)
	}
}
