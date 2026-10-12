package agentkickoff

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/provision"
)

// fakeRuntime is an agent runtime's /v1/responses endpoint, speaking the event
// stream internal/agents' responses transport reads. It records what it was sent
// and refuses any bearer but the one the row's token derives.
type fakeRuntime struct {
	mu       sync.Mutex
	bearer   string
	reply    string
	bodies   []string
	auth     []string
	sessions []string
}

// expect sets the credential the fake accepts.
func (f *fakeRuntime) expect(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bearer = v
}

// derivedCredential is what the production runtime derives from the row's token —
// the same function the gateway uses, so the fake checks the real derivation.
func derivedCredential(a agents.Agent) string { return agentgateway.HooksSHA256().Bearer(a.HooksToken) }

func (f *fakeRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, r.Method+" "+r.URL.Path+" "+string(body))
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	want, reply := "Bearer "+f.bearer, f.reply
	f.mu.Unlock()
	if r.URL.Path != "/v1/responses" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != want {
		http.Error(w, "bad bearer", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "event: response.output_text.delta\n")
	fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":%q}\n\n", reply)
	fmt.Fprint(w, "event: response.completed\n")
	fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\","+
		"\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}]}}\n\n", reply)
}

// TestADispatchedAgentAnswersItsFirstTurnThroughTheRealPlane is the goal statement
// at the tier this repository can reach: a dispatch WITH a kickoff provisions an
// agent that ANSWERS its first turn — kicked_off set, kickoff_error empty, the
// reply in its transcript.
//
// 🔴 EVERY PIECE IS THE PRODUCTION ONE EXCEPT THE RUNTIME. The real Postgres store,
// the real lifecycle adapter (over the noop driver, which reports a created
// instance ready and resolves its address from the spec's declared port), the real
// deliverer and the real agentgateway.Gateway with the real bearer derivation. Each
// is tested alone elsewhere; this is the seam none of those tests builds — the
// adapter's minted token reaching the gateway's bearer, the spec's port reaching
// the driver's endpoint, the row the adapter left `provisioning` reaching the
// deliverer's decision table. A fake on any of those sides would only restate what
// its author believed the other side does.
//
// ⚠ WHAT IT CANNOT SHOW: that a real runtime image answers, that a real kubelet
// passes the probe, that a Service name resolves. That is the cluster check named
// at the foot of cmd/muster-server/doc_seams.go entry 1.
func TestADispatchedAgentAnswersItsFirstTurnThroughTheRealPlane(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 60*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := agents.NewPG(pool)

	rt := &fakeRuntime{reply: "READY: 3 open issues"}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	driver, err := provision.NewNoop(provision.NoopEndpointTemplate(host))
	if err != nil {
		t.Fatalf("noop: %v", err)
	}
	adapter, err := agentprovision.New(agentprovision.Config{
		Driver: driver, Store: store,
		Spec: agentspec.Config{
			ImageRepo: "registry.example.test/muster/agent-runtime", APIBaseURL: "http://muster.example.test:8105",
			GatewayPort: port,
		},
		KickoffDeliverable: true,
	})
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	gw, err := agentgateway.New(agentgateway.Config{
		Driver: driver, Runtime: agentgateway.HooksSHA256(), Model: "runtime-sentinel", Client: srv.Client(),
	})
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}

	prefix := "seam-" + time.Now().Format("150405") + "-"
	const note = "list the open issues and say READY"
	row, err := store.Create(ctx, agents.Agent{
		Name: "brisk-plover", Namespace: prefix + "brisk-plover", Status: agents.StatusProvisioning,
		PendingNote: note,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(dbtest.AsOperator(context.Background()), `DELETE FROM agents WHERE id=$1`, row.ID)
	})

	if err := adapter.Dispatch(dbtest.AsOperator(context.Background()), row.ID, true); err != nil {
		t.Fatalf("Dispatch(kickoff=true) on a delivering plane: %v", err)
	}
	minted, err := store.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if minted.HooksToken == "" || minted.KickedOff || minted.KickoffError != "" {
		t.Fatalf("after Dispatch: token minted=%t kicked_off=%t kickoff_error=%q; want a token, "+
			"not kicked off, no error (the adapter must leave the turn to the deliverer)",
			minted.HooksToken != "", minted.KickedOff, minted.KickoffError)
	}
	rt.expect(derivedCredential(minted))

	d, err := New(Config{Store: store, Instances: adapter, Gateway: gw, NamespacePrefix: prefix, Owner: "seam/1"})
	if err != nil {
		t.Fatalf("deliverer: %v", err)
	}
	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	d.Wait()

	got, err := store.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	rt.mu.Lock()
	bodies, auth := append([]string(nil), rt.bodies...), append([]string(nil), rt.auth...)
	rt.mu.Unlock()
	if len(bodies) != 1 || !strings.HasPrefix(bodies[0], "POST /v1/responses ") || !strings.Contains(bodies[0], note) {
		t.Fatalf("the runtime saw %d request(s), want exactly one POST /v1/responses carrying the "+
			"pending note: %q (kickoff_error=%q)", len(bodies), bodies, got.KickoffError)
	}
	if auth[0] != "Bearer "+derivedCredential(minted) {
		t.Errorf("the turn did not carry the bearer derived from the row's minted token")
	}
	if !got.KickedOff || got.KickoffError != "" || got.KickoffAttempts != 1 || got.KickoffPod == "" {
		t.Errorf("after delivery: kicked_off=%t kickoff_error=%q attempts=%d pod=%q; want "+
			"true / \"\" / 1 / the recipient", got.KickedOff, got.KickoffError, got.KickoffAttempts, got.KickoffPod)
	}
	if agents.KickoffOwed(got) {
		t.Errorf("the row still reports a kickoff OWED after its first turn was answered")
	}
	sess, err := store.LatestOrCreateSession(ctx, row.ID, row.Name)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	msgs, err := store.ListChatMessages(ctx, sess.ID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	var transcript []string
	for _, m := range msgs {
		transcript = append(transcript, m.Role+": "+m.Content)
	}
	want := []string{"user: " + note, "assistant: READY: 3 open issues"}
	if strings.Join(transcript, "\n") != strings.Join(want, "\n") {
		t.Errorf("transcript = %q, want %q", transcript, want)
	}

	// And a second tick pays nothing: the row is kicked off.
	if err := d.Tick(ctx); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	d.Wait()
	rt.mu.Lock()
	n := len(rt.bodies)
	rt.mu.Unlock()
	if n != 1 {
		t.Errorf("a second tick sent %d more request(s) for an agent already kicked off", n-1)
	}
}
