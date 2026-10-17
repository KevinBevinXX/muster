package uploads

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func timeAt(unix int64) time.Time { return time.Unix(unix, 0) }

// AWS's own published Signature Version 4 examples for S3 (the "Examples:
// Signature Calculations in AWS Signature Version 4" page of the S3 API
// reference): the credential, the request and the expected signature are
// AWS's, so this pins signV4 to the specification rather than to a second copy
// of itself.
const (
	awsAccessKey = "AKIAIOSFODNN7EXAMPLE"
	awsSecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

var awsExampleTime = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

func sigOf(t *testing.T, req *http.Request) string {
	t.Helper()
	auth := req.Header.Get("Authorization")
	i := strings.Index(auth, "Signature=")
	if i < 0 {
		t.Fatalf("no signature in %q", auth)
	}
	return auth[i+len("Signature="):]
}

// TestSigV4MatchesAWSsGetObjectExample: GET /test.txt with a Range header.
func TestSigV4MatchesAWSsGetObjectExample(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	req.Header.Set("X-Amz-Content-Sha256", emptySHA256)
	signV4(req, emptySHA256, awsAccessKey, awsSecretKey, "us-east-1", awsExampleTime)
	if got, want := sigOf(t, req), "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"; got != want {
		t.Errorf("signature = %s; want AWS's %s", got, want)
	}
	if !strings.Contains(req.Header.Get("Authorization"), "Credential="+awsAccessKey+"/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date,") {
		t.Errorf("Authorization = %q", req.Header.Get("Authorization"))
	}
}

// TestSigV4MatchesAWSsPutObjectExample: PUT /test$file.text — a key that needs
// URI-encoding — with a payload hash and a signed Date header.
func TestSigV4MatchesAWSsPutObjectExample(t *testing.T) {
	body := []byte("Welcome to Amazon S3.")
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	if payload != "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072" {
		t.Fatalf("payload hash %s does not match AWS's example", payload)
	}
	req, _ := http.NewRequest(http.MethodPut, "https://examplebucket.s3.amazonaws.com/test$file.text", bytes.NewReader(body))
	req.Header.Set("Date", "Fri, 24 May 2013 00:00:00 GMT")
	req.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")
	req.Header.Set("X-Amz-Content-Sha256", payload)
	signV4(req, payload, awsAccessKey, awsSecretKey, "us-east-1", awsExampleTime)
	if got, want := sigOf(t, req), "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"; got != want {
		t.Errorf("signature = %s; want AWS's %s", got, want)
	}
}

// fakeS3 is a path-style S3 for one bucket. It refuses a PUT whose body does
// not hash to its x-amz-content-sha256 (as S3 and MinIO do) and any request
// not signed with SigV4 for its access key.
type fakeS3 struct {
	mu     sync.Mutex
	bucket string
	objs   map[string][]byte
	types  map[string]string
	reqs   []string
}

func newFakeS3(t *testing.T, bucket string) (*fakeS3, *httptest.Server) {
	f := &fakeS3{bucket: bucket, objs: map[string][]byte{}, types: map[string]string{}}
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	return f, ts
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r.Method+" "+r.URL.EscapedPath())
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=test-access/") {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
		return
	}
	prefix := "/" + f.bucket + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "<Error><Code>NoSuchBucket</Code></Error>")
		return
	}
	key := strings.TrimPrefix(r.URL.Path, prefix)
	switch r.Method {
	case http.MethodHead:
		if key == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != r.Header.Get("X-Amz-Content-Sha256") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "<Error><Code>XAmzContentSHA256Mismatch</Code></Error>")
			return
		}
		f.objs[key], f.types[key] = b, r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodGet:
		b, ok := f.objs[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "<Error><Code>NoSuchKey</Code></Error>")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		_, _ = w.Write(b)
		return
	case http.MethodDelete:
		delete(f.objs, key)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func testS3(t *testing.T, endpoint, access string) *S3 {
	t.Helper()
	c, err := NewS3(S3Config{Endpoint: endpoint, Bucket: "muster-uploads", AccessKey: access, SecretKey: "test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestTheS3ClientPutsGetsAndDeletes, against a fake that checks the payload
// hash and the signature's presence.
func TestTheS3ClientPutsGetsAndDeletes(t *testing.T) {
	f, ts := newFakeS3(t, "muster-uploads")
	c := testS3(t, ts.URL, "test-access")
	ctx := context.Background()
	if err := c.Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}
	data := append([]byte(nil), pngHead...)
	if err := c.Put(ctx, "uploads/"+sid, data, "image/png"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if f.types["uploads/"+sid] != "image/png" {
		t.Errorf("stored type %q", f.types["uploads/"+sid])
	}
	body, size, err := c.Get(ctx, "uploads/"+sid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(body)
	body.Close()
	if !bytes.Equal(got, data) || size != int64(len(data)) {
		t.Errorf("Get = %d bytes (size %d); want the %d put", len(got), size, len(data))
	}
	if err := c.Delete(ctx, "uploads/"+sid); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := c.Get(ctx, "uploads/"+sid); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: %v; want ErrNotFound", err)
	}
	if want := []string{"HEAD /muster-uploads/", "PUT /muster-uploads/uploads/" + sid, "GET /muster-uploads/uploads/" + sid,
		"DELETE /muster-uploads/uploads/" + sid, "GET /muster-uploads/uploads/" + sid}; strings.Join(f.reqs, "|") != strings.Join(want, "|") {
		t.Errorf("requests = %v; want %v", f.reqs, want)
	}
}

// TestAnS3RefusalIsAnErrorNamingItsCode: a refused credential is an S3Error
// carrying S3's code, and the secret is in no error.
func TestAnS3RefusalIsAnErrorNamingItsCode(t *testing.T) {
	_, ts := newFakeS3(t, "muster-uploads")
	c := testS3(t, ts.URL, "wrong-access")
	err := c.Put(context.Background(), "uploads/"+sid, []byte("x"), "text/plain")
	var se *S3Error
	if !errors.As(err, &se) || se.Status != http.StatusForbidden || se.Code != "AccessDenied" {
		t.Fatalf("Put with a wrong key: %v; want S3Error 403 AccessDenied", err)
	}
	if strings.Contains(err.Error(), "test-secret") {
		t.Errorf("the error carries the secret: %v", err)
	}
	if err := c.Check(context.Background()); err == nil {
		t.Error("Check with a wrong key answered nil")
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
