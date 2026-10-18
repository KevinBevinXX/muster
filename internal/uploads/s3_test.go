package uploads

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

// TestCoverage: only an ENABLED rule whose prefix covers uploads/ counts, per
// action — expiring objects, and aborting incomplete multipart uploads.
func TestCoverage(t *testing.T) {
	exp := lifecycle.Expiration{Days: 8}
	abort := lifecycle.AbortIncompleteMultipartUpload{DaysAfterInitiation: 1}
	for name, tc := range map[string]struct {
		rules []lifecycle.Rule
		want  LifecycleCoverage
	}{
		"none":                {nil, LifecycleCoverage{}},
		"whole bucket":        {[]lifecycle.Rule{{ID: "a", Status: "Enabled", Expiration: exp}}, LifecycleCoverage{Expire: 1}},
		"uploads prefix":      {[]lifecycle.Rule{{ID: "a", Status: "Enabled", Expiration: exp, RuleFilter: lifecycle.Filter{Prefix: "uploads/"}}}, LifecycleCoverage{Expire: 1}},
		"both in one rule":    {[]lifecycle.Rule{{ID: "a", Status: "Enabled", Expiration: exp, AbortIncompleteMultipartUpload: abort, RuleFilter: lifecycle.Filter{Prefix: "uploads/"}}}, LifecycleCoverage{Expire: 1, AbortIncomplete: 1}},
		"abort only":          {[]lifecycle.Rule{{ID: "a", Status: "Enabled", AbortIncompleteMultipartUpload: abort}}, LifecycleCoverage{AbortIncomplete: 1}},
		"another prefix":      {[]lifecycle.Rule{{ID: "a", Status: "Enabled", Expiration: exp, AbortIncompleteMultipartUpload: abort, RuleFilter: lifecycle.Filter{Prefix: "backups/"}}}, LifecycleCoverage{}},
		"disabled":            {[]lifecycle.Rule{{ID: "a", Status: "Disabled", Expiration: exp, AbortIncompleteMultipartUpload: abort}}, LifecycleCoverage{}},
		"no action":           {[]lifecycle.Rule{{ID: "a", Status: "Enabled"}}, LifecycleCoverage{}},
		"legacy prefix field": {[]lifecycle.Rule{{ID: "a", Status: "Enabled", Expiration: exp, Prefix: "uploads/"}}, LifecycleCoverage{Expire: 1}},
	} {
		if got := Coverage(&lifecycle.Configuration{Rules: tc.rules}); got != tc.want {
			t.Errorf("%s: %+v; want %+v", name, got, tc.want)
		}
	}
}

// failAfter yields n bytes of data, then fails with err (calling hook first).
type failAfter struct {
	n    int
	err  error
	hook func()
}

func (f *failAfter) Read(p []byte) (int, error) {
	if f.n <= 0 {
		if f.hook != nil {
			f.hook()
			f.hook = nil
		}
		return 0, f.err
	}
	k := min(len(p), f.n)
	for i := range p[:k] {
		p[i] = byte('a' + i%26)
	}
	f.n -= k
	return k, nil
}
