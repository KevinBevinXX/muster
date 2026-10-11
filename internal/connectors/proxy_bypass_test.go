package connectors_test

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
)

// The ways a REFLECTING upstream could hand the credential back past the
// redactor, each driven through the proxy and asserted on what a real client
// receives. Redaction is defence in depth (see secretForms in proxy.go); these
// pin the specific bypasses it was measured against.

// front serves the fixture's proxy over real HTTP, so a test sees what a real
// client receives — 1xx responses and trailers included, which an
// httptest.ResponseRecorder does not model.
func (f *fixture) front(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(f.handler)
	t.Cleanup(s.Close)
	return s.URL
}

// carriesSecret reports whether s holds the fixture credential in plain or in
// the basic pair's base64.
func carriesSecret(s, secret string) bool {
	pair := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + secret))
	return strings.Contains(strings.ToLower(s), strings.ToLower(secret)) || strings.Contains(s, strings.TrimRight(pair, "="))
}

// TestRangeCannotSliceAReflectedCredential: an upstream that honours Range would
// serve a reflected credential in slices shorter than anything the redactor
// matches; rejoined, they are the credential. Range never reaches the upstream.
func TestRangeCannotSliceAReflectedCredential(t *testing.T) {
	f := newFixture(t, connectors.SchemeBearer)
	f.approve(t, f.agentA)
	var sawRange atomic.Bool
	f.up.handle = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" || r.Header.Get("If-Range") != "" {
			sawRange.Store(true)
		}
		body := "echo:" + r.Header.Get("Authorization") + ":end"
		http.ServeContent(w, r, "echo.txt", time.Time{}, strings.NewReader(body))
	}
	var joined strings.Builder
	partial := 0
	for start := 0; start < 400; start += 5 {
		w := f.do(t, "GET", "/connect/svc/"+f.host+"/echo", http.Header{
			"Range": {fmt.Sprintf("bytes=%d-%d", start, start+4)},
		})
		if w.Code == http.StatusRequestedRangeNotSatisfiable {
			break
		}
		joined.WriteString(w.Body.String())
		if w.Code != http.StatusPartialContent {
			break
		}
		partial++
	}
	if carriesSecret(joined.String(), fakeSecret) {
		t.Fatalf("the credential was reassembled from %d ranged slice(s): %q", partial, joined.String())
	}
	f.do(t, "GET", "/connect/svc/"+f.host+"/echo", http.Header{"Range": {"bytes=0-4"}, "If-Range": {`"etag"`}})
	if sawRange.Load() {
		t.Fatal("a Range/If-Range header reached the upstream")
	}
	if !strings.Contains(joined.String(), connectors.RedactedMarker) {
		t.Fatalf("control: the full reflected body did not come back redacted: %q", joined.String())
	}
}

// TestTheProxyRefusesMethodsItCannotJustify: TRACE (an echo of the request,
// injected credential included), CONNECT and invented methods are refused
// before the upstream is reached or a request is filed.
func TestTheProxyRefusesMethodsItCannotJustify(t *testing.T) {
	f := newFixture(t, connectors.SchemeBearer)
	f.approve(t, f.agentA)
	for _, m := range []string{"TRACE", "TRACK", "CONNECT", "PROPFIND", "DEBUG"} {
		w := f.do(t, m, "/connect/svc/"+f.host+"/x", nil)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d %s, want 405", m, w.Code, w.Body)
		}
	}
	if n := f.up.hits.Load(); n != 0 {
		t.Fatalf("the upstream received %d request(s) for refused methods", n)
	}
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		if w := f.do(t, m, "/connect/svc/"+f.host+"/x", nil); w.Code != http.StatusOK {
			t.Errorf("control: %s status %d %s, want 200", m, w.Code, w.Body)
		}
	}
}

// TestUpgradesAreRefused: a WebSocket/h2c upgrade would make the response a raw
// byte stream the redactor never reads.
func TestUpgradesAreRefused(t *testing.T) {
	f := newFixture(t, connectors.SchemeBearer)
	f.approve(t, f.agentA)
	w := f.do(t, "GET", "/connect/svc/"+f.host+"/ws", http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}})
	if w.Code != http.StatusBadRequest || f.up.hits.Load() != 0 {
		t.Fatalf("status %d %s, upstream hits %d; want 400 and none", w.Code, w.Body, f.up.hits.Load())
	}
}

// TestInformationalResponsesNeverReachTheClient: a 103 Early Hints (any 1xx)
// is relayed by httputil.ReverseProxy without passing ModifyResponse, so its
// headers would skip redaction and the Set-Cookie strip.
func TestInformationalResponsesNeverReachTheClient(t *testing.T) {
	f := newFixture(t, connectors.SchemeBearer)
	f.approve(t, f.agentA)
	f.up.handle = func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("Link", "<https://cdn.example.com/?k="+auth+">; rel=preload")
		w.Header().Set("Set-Cookie", "s="+strings.TrimPrefix(auth, "Bearer "))
		w.Header().Set("X-Hint", auth)
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		w.Header().Del("Set-Cookie")
		w.Header().Del("X-Hint")
		_, _ = io.WriteString(w, "final")
	}
	var mu sync.Mutex
	var got []string
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, fmt.Sprintf("%d %v", code, h))
			return nil
		},
	})
	req, _ := http.NewRequestWithContext(ctx, "GET", f.front(t)+"/connect/svc/"+f.host+"/hints", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "final" {
		t.Fatalf("control: status %d body %q", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("the client received %d informational response(s); leaks credential: %v — %v",
			len(got), carriesSecret(strings.Join(got, " "), fakeSecret), got)
	}
}

// TestTrailersNeverReachTheClient: trailers arrive after the body was streamed
// and are not redacted, so none are forwarded — declared or not.
func TestTrailersNeverReachTheClient(t *testing.T) {
	f := newFixture(t, connectors.SchemeBearer)
	f.approve(t, f.agentA)
	f.up.handle = func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("Trailer", "X-Echo")
		_, _ = io.WriteString(w, "body")
		w.Header().Set("X-Echo", auth)
		w.Header().Set(http.TrailerPrefix+"X-Undeclared", auth)
	}
	resp, err := http.Get(f.front(t) + "/connect/svc/" + f.host + "/trailers")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "body" {
		t.Fatalf("control: status %d body %q", resp.StatusCode, body)
	}
	if len(resp.Trailer) != 0 {
		var all []string
		for k, vs := range resp.Trailer {
			all = append(all, k+": "+strings.Join(vs, ","))
		}
		t.Fatalf("the client received trailers (credential: %v): %v", carriesSecret(strings.Join(all, " "), fakeSecret), all)
	}
}

// TestACompressedBodyIsRefusedNotForwardedUnread: the proxy asks for identity;
// an upstream that compresses anyway gets a 502, because the redactor would be
// scanning compressed bytes the client then inflates.
func TestACompressedBodyIsRefusedNotForwardedUnread(t *testing.T) {
	for _, enc := range []string{"deflate", "gzip", "br"} {
		t.Run(enc, func(t *testing.T) {
			f := newFixture(t, connectors.SchemeBearer)
			f.approve(t, f.agentA)
			var askedFor atomic.Value
			f.up.handle = func(w http.ResponseWriter, r *http.Request) {
				askedFor.Store(r.Header.Get("Accept-Encoding"))
				var buf bytes.Buffer
				switch enc {
				case "deflate":
					zw, _ := flate.NewWriter(&buf, flate.BestCompression)
					_, _ = io.WriteString(zw, "auth="+r.Header.Get("Authorization"))
					_ = zw.Close()
				default:
					zw := gzip.NewWriter(&buf)
					_, _ = io.WriteString(zw, "auth="+r.Header.Get("Authorization"))
					_ = zw.Close()
				}
				w.Header().Set("Content-Encoding", enc)
				_, _ = w.Write(buf.Bytes())
			}
			w := f.do(t, "GET", "/connect/svc/"+f.host+"/z", http.Header{"Accept-Encoding": {"deflate, gzip, br"}})
			leaked := false
			if enc == "deflate" {
				if plain, err := io.ReadAll(flate.NewReader(bytes.NewReader(w.Body.Bytes()))); err == nil {
					leaked = carriesSecret(string(plain), fakeSecret)
				}
			}
			if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "compressed") {
				t.Fatalf("status %d (inflated body carries the credential: %v), want 502 naming the compression", w.Code, leaked)
			}
			if got, _ := askedFor.Load().(string); got != "identity" {
				t.Fatalf("the upstream was sent Accept-Encoding %q, want identity", got)
			}
			assertNoSecret(t, w)
		})
	}
}

// TestReEncodedReflectionsAreRedacted: an upstream that reflects the header in
// another encoding — JSON-escaped, case-changed, base64 at an offset inside a
// larger blob, hex, percent-encoded — has each form redacted. The secret
// carries characters JSON and URL encoding rewrite, so every form below is
// distinct from the plain secret (the control checks that).
func TestReEncodedReflectionsAreRedacted(t *testing.T) {
	const secret = "FAKE/sec<ret>&key+0123456789"
	f := newFixtureWithSecret(t, connectors.SchemeBearer, secret)
	f.approve(t, f.agentA)
	var mu sync.Mutex
	emitted := map[string]string{}
	f.up.handle = func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		jsonHTML, _ := json.Marshal(auth)
		var noHTML bytes.Buffer
		enc := json.NewEncoder(&noHTML)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(auth)
		var pct strings.Builder
		for _, b := range []byte(secret) {
			fmt.Fprintf(&pct, "%%%02x", b)
		}
		forms := map[string]string{
			"json-html":   strings.Trim(string(jsonHTML), `"`),
			"json-slash":  strings.ReplaceAll(strings.Trim(strings.TrimSpace(noHTML.String()), `"`), "/", `\/`),
			"upper":       strings.ToUpper(auth),
			"lower":       strings.ToLower(auth),
			"b64-line":    base64.StdEncoding.EncodeToString([]byte("Authorization: " + auth + "\r\n")),
			"b64-off1":    base64.StdEncoding.EncodeToString([]byte("x" + auth)),
			"b64url-off0": base64.RawURLEncoding.EncodeToString([]byte("ab" + auth)),
			"hex-upper":   strings.ToUpper(hex.EncodeToString([]byte(secret))),
			"pct-lower":   pct.String(),
			"query":       url.QueryEscape(auth),
		}
		mu.Lock()
		for k, v := range forms {
			emitted[k] = v
		}
		mu.Unlock()
		for k, v := range forms {
			fmt.Fprintf(w, "%s=%s\n", k, v)
		}
	}
	w := f.do(t, "GET", "/connect/svc/"+f.host+"/reflect", nil)
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	lines := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(w.Body.String()), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			lines[k] = v
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(emitted) != 10 {
		t.Fatalf("control: the upstream emitted %d forms", len(emitted))
	}
	for k, v := range emitted {
		if k != "upper" && k != "lower" && strings.Contains(v, secret) {
			t.Fatalf("control: form %s carries the plain secret, so it tests nothing new", k)
		}
		got := lines[k]
		if strings.Contains(w.Body.String(), v) || !strings.Contains(got, connectors.RedactedMarker) {
			t.Errorf("form %s survived redaction: sent %q, client got %q", k, v, got)
		}
	}
}

// TestTheFirstConcurrentRequestsShareOneTransport: the upstream transport is
// built once even when the first requests arrive together (run under -race,
// as `make test` and CI do).
//
// The store is a stub with NO locking on purpose: a real store's connection
// pool synchronises the goroutines on the way in and hides the race from the
// detector (measured: the database-backed version of this test passed against
// the unsynchronised code).
func TestTheFirstConcurrentRequestsShareOneTransport(t *testing.T) {
	up := newUpstream(t)
	host := "api.fake.test:" + up.port()
	p := &connectors.Proxy{Store: approvedStub{c: connectors.Connector{Name: "svc", Hosts: []string{host},
		Scheme: connectors.SchemeBearer}}, Policy: testPolicy()}
	var wg sync.WaitGroup
	codes := make([]int, 16)
	start := make(chan struct{})
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r := httptest.NewRequest("GET", "/connect/svc/"+host+"/x", nil)
			w := httptest.NewRecorder()
			p.Serve(w, r, connectors.Caller{AgentID: 1, AgentName: "a"}, "svc", host, "x")
			codes[i] = w.Code
		}(i)
	}
	close(start)
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("request %d: status %d", i, c)
		}
	}
}

// approvedStub is a Store with an approved grant and a fixed credential, and no
// synchronisation of its own. Methods the proxy does not call panic (the nil
// embedded interface).
type approvedStub struct {
	connectors.Store
	c connectors.Connector
}

func (s approvedStub) LatestGrant(context.Context, int64, string) (connectors.Grant, error) {
	return connectors.Grant{ID: 1, Status: connectors.StatusApproved}, nil
}
func (s approvedStub) Credential(context.Context, string) (connectors.Connector, string, error) {
	return s.c, fakeSecret, nil
}
func (approvedStub) Audit(context.Context, connectors.AuditEntry) error { return nil }
func (approvedStub) TouchGrant(context.Context, int64) error            { return nil }
func (approvedStub) TouchConnector(context.Context, string) error       { return nil }

// TestPendingRequestsAreCappedPerAgent: past the cap (5) a request for yet
// another connector is refused with 429 — nothing filed, nobody prompted — while
// a request for a connector the agent already asked for still answers with
// that pending grant, and another agent is unaffected.
func TestPendingRequestsAreCappedPerAgent(t *testing.T) {
	const capPerAgent = 5
	f := newFixture(t, connectors.SchemeBearer)
	for i := 0; i < capPerAgent; i++ {
		w := f.do(t, "GET", fmt.Sprintf("/connect/want-%d/%s/x", i, f.host), nil)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"grant":"pending"`) {
			t.Fatalf("request %d: status %d %s, want 403 pending", i, w.Code, w.Body)
		}
	}
	w := f.do(t, "GET", "/connect/one-too-many/"+f.host+"/x", nil)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "waiting") {
		t.Fatalf("past the cap: status %d %s, want 429", w.Code, w.Body)
	}
	if n := f.filed.Load(); n != capPerAgent {
		t.Fatalf("the operator was prompted %d times, want %d", n, capPerAgent)
	}
	if _, err := f.store.LatestGrant(f.ctx, f.agentA.AgentID, "one-too-many"); !errors.Is(err, connectors.ErrNotFound) {
		t.Fatalf("a grant row was filed past the cap: %v", err)
	}
	// An existing request is still answered as it stands.
	if w := f.do(t, "GET", "/connect/want-0/"+f.host+"/x", nil); w.Code != http.StatusForbidden ||
		!strings.Contains(w.Body.String(), `"grant":"pending"`) {
		t.Fatalf("an existing pending request: status %d %s", w.Code, w.Body)
	}
	// The cap is per agent.
	if w := f.do(t, "GET", "/connect/one-too-many/"+f.host+"/x", http.Header{"X-Test-Agent": {"b"}}); w.Code != http.StatusForbidden {
		t.Fatalf("agent B: status %d %s, want its own 403 pending", w.Code, w.Body)
	}
	if f.up.hits.Load() != 0 {
		t.Fatal("the upstream was reached without a grant")
	}
}

// TestTheCapHoldsUnderConcurrentRequests: many simultaneous requests for
// different connectors from one agent still file at most the cap.
func TestTheCapHoldsUnderConcurrentRequests(t *testing.T) {
	const capPerAgent = 5
	f := newFixture(t, connectors.SchemeBearer)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _ = f.store.RequestGrant(f.ctx, f.agentA.AgentID, fmt.Sprintf("race-%d", i), "")
		}(i)
	}
	wg.Wait()
	pending, err := f.store.ListPendingGrants(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != capPerAgent {
		t.Fatalf("%d pending requests after 20 concurrent asks, want exactly %d", len(pending), capPerAgent)
	}
}

// TestATamperedRowSendsNothing: a database writer without the key edits the
// fields that decide where and how the secret goes. Each edit makes the
// ciphertext fail to open: the proxy answers 502 and no upstream — the real one
// or the one the edit points at — receives a request.
func TestATamperedRowSendsNothing(t *testing.T) {
	evil := newUpstream(t)
	evilHost := "evil.fake.test:" + evil.port()
	for name, tc := range map[string]struct {
		sql  string
		host string // the host the attacker would then call through
	}{
		"host added":    {`UPDATE connectors SET hosts = array_append(hosts, $1) WHERE name='svc'`, evilHost},
		"host replaced": {`UPDATE connectors SET hosts = ARRAY[$1] WHERE name='svc'`, evilHost},
		"scheme":        {`UPDATE connectors SET auth_scheme='bearer', auth_username='' WHERE name='svc' AND $1 <> ''`, ""},
		"header":        {`UPDATE connectors SET auth_scheme='header', auth_header='X-Leak', auth_username='' WHERE name='svc' AND $1 <> ''`, ""},
		"username":      {`UPDATE connectors SET auth_username='someone-else' WHERE name='svc' AND $1 <> ''`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, connectors.SchemeBasic)
			f.approve(t, f.agentA)
			if w := f.do(t, "GET", "/connect/svc/"+f.host+"/x", nil); w.Code != 200 {
				t.Fatalf("control: before the edit, status %d %s", w.Code, w.Body)
			}
			before := f.up.hits.Load()
			tag, err := f.pool.Exec(f.ctx, tc.sql, evilHost)
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("tamper: %v (%d rows)", err, tag.RowsAffected())
			}
			host := f.host
			if tc.host != "" {
				host = tc.host
			}
			w := f.do(t, "GET", "/connect/svc/"+host+"/x", nil)
			if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "decrypted") {
				t.Fatalf("after the edit: status %d %s, want 502 (decrypt)", w.Code, w.Body)
			}
			if n := f.up.hits.Load() - before; n != 0 || evil.hits.Load() != 0 {
				t.Fatalf("after the edit the real upstream got %d and the edit's target %d request(s)", n, evil.hits.Load())
			}
			assertNoSecret(t, w)
			// A rotation cannot launder the edit into a fresh seal.
			if err := f.store.RotateSecret(f.ctx, "svc", "ghp_FAKE-rotated-0123456789"); !errors.Is(err, connectors.ErrDecrypt) {
				t.Fatalf("rotate after the edit = %v, want ErrDecrypt", err)
			}
		})
	}
}
