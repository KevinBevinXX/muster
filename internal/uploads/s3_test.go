package uploads

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7/pkg/lifecycle"
)

func timeAt(unix int64) time.Time { return time.Unix(unix, 0) }

// slowS3 answers a path-style GET of one object: its headers at once (or
// after headerDelay), then the body in `chunks` pieces `gap` apart. PUTs are
// refused AccessDenied, as a store refuses a wrong key.
type slowS3 struct {
	body        []byte
	chunks      int
	gap         time.Duration
	headerDelay time.Duration
}

func (f *slowS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied.</Message></Error>`)
		return
	}
	time.Sleep(f.headerDelay)
	w.Header().Set("Content-Length", strconv.Itoa(len(f.body)))
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("ETag", `"0123456789abcdef0123456789abcdef"`)
	w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
	step := (len(f.body) + f.chunks - 1) / f.chunks
	for i := 0; i < len(f.body); i += step {
		end := min(i+step, len(f.body))
		_, _ = w.Write(f.body[i:end])
		w.(http.Flusher).Flush()
		time.Sleep(f.gap)
	}
}

func testS3(t *testing.T, endpoint string, header time.Duration) *S3 {
	t.Helper()
	c, err := NewS3(S3Config{Endpoint: endpoint, Bucket: "muster-uploads", AccessKey: "test-access", SecretKey: "test-secret",
		HeaderTimeout: header})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestASlowDownloadIsNotCutOffMidFile: the store's client bounds only the wait
// for HEADERS. A body that takes many times that header timeout to arrive —
// an agent reading slowly through GET /f/{id} — still arrives whole.
// (Measured against the old client, whose total 2-minute Timeout covered the
// body: with that bound set to the header timeout here, this read fails.)
func TestASlowDownloadIsNotCutOffMidFile(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 4000)
	ts := httptest.NewServer(&slowS3{body: body, chunks: 8, gap: 100 * time.Millisecond})
	t.Cleanup(ts.Close)
	c := testS3(t, ts.URL, 150*time.Millisecond)
	start := time.Now()
	rc, size, err := c.Get(context.Background(), "uploads/"+sid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, body) || size != int64(len(body)) {
		t.Fatalf("read %d of %d bytes (size %d), err %v", len(got), len(body), size, err)
	}
	if took := time.Since(start); took < 4*150*time.Millisecond {
		t.Fatalf("CONTROL: the body arrived in %v, not slower than the header timeout, so this proves nothing", took)
	}
}

// TestAStoreThatNeverAnswersFailsAtTheHeaderTimeout: the other half — the
// header timeout is real.
func TestAStoreThatNeverAnswersFailsAtTheHeaderTimeout(t *testing.T) {
	ts := httptest.NewServer(&slowS3{body: []byte("x"), chunks: 1, headerDelay: 5 * time.Second})
	t.Cleanup(ts.Close)
	c := testS3(t, ts.URL, 150*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	if _, _, err := c.Get(ctx, "uploads/"+sid); err == nil {
		t.Fatal("Get from a store that never answers succeeded")
	}
	// Three tries of 150ms each, and minio-go's backoff between them: well
	// under the 5s the store stays silent for.
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("a silent store was waited on for %v; the header timeout is 150ms (×3 tries)", took)
	}
}

// TestAnS3RefusalIsAnErrorNamingItsCode: the store's refusal is an S3Error
// carrying S3's code, and the secret is in no error.
func TestAnS3RefusalIsAnErrorNamingItsCode(t *testing.T) {
	ts := httptest.NewServer(&slowS3{body: []byte("x"), chunks: 1})
	t.Cleanup(ts.Close)
	c := testS3(t, ts.URL, 0)
	err := c.Put(context.Background(), "uploads/"+sid, bytes.NewReader([]byte("hello")), 5, "text/plain")
	var se *S3Error
	if !errors.As(err, &se) || se.Status != http.StatusForbidden || se.Code != "AccessDenied" {
		t.Fatalf("a refused Put: %v; want S3Error 403 AccessDenied", err)
	}
	if strings.Contains(err.Error(), "test-secret") {
		t.Errorf("the error carries the secret: %v", err)
	}
}

// TestNewS3RefusesABadConfiguration: the shapes buildUploads reports as
// MISCONFIGURED.
func TestNewS3RefusesABadConfiguration(t *testing.T) {
	ok := S3Config{Endpoint: "http://minio.example.test:9000", Bucket: "muster-uploads", AccessKey: "a", SecretKey: "s"}
	if _, err := NewS3(ok); err != nil {
		t.Fatalf("CONTROL: a good config was refused: %v", err)
	}
	for name, mut := range map[string]func(*S3Config){
		"no scheme":        func(c *S3Config) { c.Endpoint = "minio.example.test:9000" },
		"a path":           func(c *S3Config) { c.Endpoint = "http://minio.example.test/bucket" },
		"credentials":      func(c *S3Config) { c.Endpoint = "http://u:p@minio.example.test" },
		"bucket caps":      func(c *S3Config) { c.Bucket = "Muster" },
		"bucket too short": func(c *S3Config) { c.Bucket = "ab" },
		"no access key":    func(c *S3Config) { c.AccessKey = "" },
		"no secret":        func(c *S3Config) { c.SecretKey = "" },
	} {
		c := ok
		mut(&c)
		if _, err := NewS3(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestCountExpiryRules: only an ENABLED rule WITH an expiration whose prefix
// covers uploads/ counts.
func TestCountExpiryRules(t *testing.T) {
	exp := lifecycle.Expiration{Days: 8}
	for name, tc := range map[string]struct {
		rules []lifecycle.Rule
		want  int
	}{
		"none":                {nil, 0},
		"whole bucket":        {[]lifecycle.Rule{{ID: "a", Status: "Enabled", Expiration: exp}}, 1},
		"uploads prefix":      {[]lifecycle.Rule{{ID: "a", Status: "Enabled", Expiration: exp, RuleFilter: lifecycle.Filter{Prefix: "uploads/"}}}, 1},
		"another prefix":      {[]lifecycle.Rule{{ID: "a", Status: "Enabled", Expiration: exp, RuleFilter: lifecycle.Filter{Prefix: "backups/"}}}, 0},
		"disabled":            {[]lifecycle.Rule{{ID: "a", Status: "Disabled", Expiration: exp}}, 0},
		"no expiration":       {[]lifecycle.Rule{{ID: "a", Status: "Enabled"}}, 0},
		"legacy prefix field": {[]lifecycle.Rule{{ID: "a", Status: "Enabled", Expiration: exp, Prefix: "uploads/"}}, 1},
	} {
		if got := CountExpiryRules(&lifecycle.Configuration{Rules: tc.rules}); got != tc.want {
			t.Errorf("%s: %d; want %d", name, got, tc.want)
		}
	}
}

// TestAgainstARealS3 runs the client against a REAL S3-compatible server when
// one is named, and skips otherwise. It is the live control behind the fakes
// above, run by hand (docs/testing.md):
//
//	MUSTER_TEST_S3_ENDPOINT=http://127.0.0.1:9000 MUSTER_TEST_S3_BUCKET=muster-test \
//	  MUSTER_TEST_S3_ACCESS_KEY=… MUSTER_TEST_S3_SECRET_KEY=… \
//	  go test -run TestAgainstARealS3 -v ./internal/uploads/
//
// (any S3-compatible server on loopback; the bucket must exist). It writes a
// small file in one request AND a file over PartSize as a multipart stream of
// unknown length, reads both back, HEADs them, and checks that a wrong secret
// is REFUSED by the server.
func TestAgainstARealS3(t *testing.T) {
	cfg := S3Config{Endpoint: os.Getenv("MUSTER_TEST_S3_ENDPOINT"), Bucket: os.Getenv("MUSTER_TEST_S3_BUCKET"),
		AccessKey: os.Getenv("MUSTER_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("MUSTER_TEST_S3_SECRET_KEY")}
	if cfg.Endpoint == "" {
		t.Skip("MUSTER_TEST_S3_ENDPOINT is unset: no real S3 to run against")
	}
	c, err := NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}
	for name, tc := range map[string]struct {
		data []byte
		size int64
	}{
		"one request": {bytes.Repeat(pngHead, 64), int64(len(pngHead) * 64)},
		"multipart":   {bytes.Repeat([]byte("0123456789abcdef"), (PartSize+PartSize/2)/16), -1},
	} {
		id, _ := NewPublicID()
		key := ObjectKeyFor(id)
		if err := c.Put(ctx, key, io.MultiReader(bytes.NewReader(tc.data)), tc.size, "application/octet-stream"); err != nil {
			t.Fatalf("%s: Put: %v", name, err)
		}
		if n, err := c.Stat(ctx, key); err != nil || n != int64(len(tc.data)) {
			t.Errorf("%s: Stat = %d, %v; want %d", name, n, err, len(tc.data))
		}
		body, size, err := c.Get(ctx, key)
		if err != nil {
			t.Fatalf("%s: Get: %v", name, err)
		}
		got, _ := io.ReadAll(body)
		body.Close()
		if !bytes.Equal(got, tc.data) || size != int64(len(tc.data)) {
			t.Errorf("%s: Get: %d bytes (size %d); want the %d put", name, len(got), size, len(tc.data))
		}
		if err := c.Delete(ctx, key); err != nil {
			t.Fatalf("%s: Delete: %v", name, err)
		}
		if _, _, err := c.Get(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: Get after Delete: %v; want ErrNotFound", name, err)
		}
		if _, err := c.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: Stat after Delete: %v; want ErrNotFound", name, err)
		}
	}
	n, lerr := c.ExpiryRules(ctx)
	t.Logf("ExpiryRules on %s: %d, %v", cfg.Bucket, n, lerr)
	bad := cfg
	bad.SecretKey += "-wrong"
	wrong, _ := NewS3(bad)
	var se *S3Error
	err = wrong.Put(ctx, ObjectKeyFor(sid), bytes.NewReader([]byte("x")), 1, "text/plain")
	if !errors.As(err, &se) || se.Status != http.StatusForbidden {
		t.Errorf("NEGATIVE CONTROL: a wrong secret was answered %v; want a 403 from the server", err)
	}
	t.Logf("real S3 at %s: one-request and multipart put/stat/get/delete OK; a wrong secret answered %v", cfg.Endpoint, se)
}
