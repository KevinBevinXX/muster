package uploads

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// A MINIMAL S3 CLIENT: four calls (PUT, GET, DELETE an object; HEAD the
// bucket), path-style addressing, AWS Signature Version 4 in the header.
//
// 🔴 WHY NOT AN SDK. muster depended on no S3 client, and the two candidates
// (aws-sdk-go-v2, minio-go) bring a module graph several times this file's size
// into a binary that otherwise needs Postgres and nothing else. What this needs
// is small and fixed, and the signature it computes is checked against AWS's own
// published example requests (s3_test.go) — not against a second copy of this
// code — and was measured against a real MinIO (see the pull request).
//
// 🔴 THE PAYLOAD IS HASHED, NOT STREAMED UNSIGNED. An upload is already held
// whole (its bytes are sniffed and digested before they are stored), so the
// PUT carries the real x-amz-content-sha256 and the store refuses a body that
// does not match it. No aws-chunked encoding, so nothing here depends on how a
// given server implements it.
// ---------------------------------------------------------------------------

// ObjectStore is what the upload service needs of a bucket.
type ObjectStore interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	// Get opens key. The caller closes the body. A missing key is ErrNotFound.
	Get(ctx context.Context, key string) (body io.ReadCloser, size int64, err error)
	Delete(ctx context.Context, key string) error
	// Check reaches the bucket with the configured credential.
	Check(ctx context.Context) error
}

// S3Config is an S3-compatible endpoint, bucket and credential.
type S3Config struct {
	// Endpoint is the server's base URL, http(s)://host[:port], no path.
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	// Region is the signing region; MinIO's default is us-east-1.
	Region string
}

// DefaultRegion is the signing region when none is configured.
const DefaultRegion = "us-east-1"

// S3 is ObjectStore over an S3-compatible HTTP API.
type S3 struct {
	cfg  S3Config
	base *url.URL
	hc   *http.Client
	now  func() time.Time
}

// NewS3 validates cfg and returns a client. The HTTP client has a 2-minute
// overall timeout: a 25 MiB object on a LAN moves in seconds.
func NewS3(cfg S3Config) (*S3, error) {
	u, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return nil, fmt.Errorf("uploads: the S3 endpoint must be http(s)://host[:port] with no path, query or credentials")
	}
	if !validBucket(cfg.Bucket) {
		return nil, fmt.Errorf("uploads: %q is not an S3 bucket name (3-63 of a-z 0-9 . -)", cfg.Bucket)
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("uploads: the S3 access key and secret key are both required")
	}
	if cfg.Region == "" {
		cfg.Region = DefaultRegion
	}
	return &S3{cfg: cfg, base: u, hc: &http.Client{Timeout: 2 * time.Minute}, now: time.Now}, nil
}

func validBucket(b string) bool {
	if len(b) < 3 || len(b) > 63 {
		return false
	}
	for _, r := range b {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-') {
			return false
		}
	}
	return true
}

// objectURL is the path-style URL of key.
func (c *S3) objectURL(key string) string {
	u := *c.base
	u.Path = "/" + c.cfg.Bucket + "/" + key
	u.RawPath = "/" + c.cfg.Bucket + "/" + uriEncode(key, false)
	return u.String()
}

// emptySHA256 is the SHA-256 of no bytes: the payload hash of a GET, DELETE
// or HEAD.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (c *S3) do(ctx context.Context, method, rawURL string, body []byte, contentType string) (*http.Response, error) {
	var rd io.Reader
	payload := emptySHA256
	if body != nil {
		rd = bytes.NewReader(body)
		sum := sha256.Sum256(body)
		payload = hex.EncodeToString(sum[:])
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("X-Amz-Content-Sha256", payload)
	signV4(req, payload, c.cfg.AccessKey, c.cfg.SecretKey, c.cfg.Region, c.now())
	return c.hc.Do(req)
}

// S3Error is a non-success answer from the store. Its message names the
// status and S3's error code, never a credential.
type S3Error struct {
	Op     string
	Status int
	Code   string
}

func (e *S3Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("uploads: S3 %s: HTTP %d (%s)", e.Op, e.Status, e.Code)
	}
	return fmt.Sprintf("uploads: S3 %s: HTTP %d", e.Op, e.Status)
}

func s3Error(op string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	code := ""
	if i := bytes.Index(b, []byte("<Code>")); i >= 0 {
		rest := b[i+len("<Code>"):]
		if j := bytes.Index(rest, []byte("</Code>")); j >= 0 && j <= 64 {
			code = string(rest[:j])
		}
	}
	return &S3Error{Op: op, Status: resp.StatusCode, Code: code}
}

// Put stores data at key.
func (c *S3) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if data == nil {
		data = []byte{}
	}
	resp, err := c.do(ctx, http.MethodPut, c.objectURL(key), data, contentType)
	if err != nil {
		return fmt.Errorf("uploads: S3 put: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return s3Error("put", resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return nil
}

// Get opens key.
func (c *S3) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	resp, err := c.do(ctx, http.MethodGet, c.objectURL(key), nil, "")
	if err != nil {
		return nil, 0, fmt.Errorf("uploads: S3 get: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, 0, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, 0, s3Error("get", resp)
	}
	return resp.Body, resp.ContentLength, nil
}

// Delete removes key. A key already gone is not an error (S3 answers 204).
func (c *S3) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, c.objectURL(key), nil, "")
	if err != nil {
		return fmt.Errorf("uploads: S3 delete: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return s3Error("delete", resp)
	}
	return nil
}

// Check is HEAD on the bucket: 200 when it exists and the credential may
// reach it.
func (c *S3) Check(ctx context.Context) error {
	u := *c.base
	u.Path = "/" + c.cfg.Bucket + "/"
	resp, err := c.do(ctx, http.MethodHead, u.String(), nil, "")
	if err != nil {
		return fmt.Errorf("uploads: S3 head bucket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &S3Error{Op: "head bucket", Status: resp.StatusCode}
	}
	return nil
}

// --- Signature Version 4 ----------------------------------------------------

// signV4 adds X-Amz-Date and Authorization to req. EVERY header already on
// req, plus Host, is signed; the caller sets exactly the ones it means to
// sign. payloadHash is the hex SHA-256 of the body (x-amz-content-sha256).
func signV4(req *http.Request, payloadHash, accessKey, secretKey, region string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{"host": host}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.Join(trimAll(v), ",")
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method,
		uriEncode(req.URL.Path, false),
		canonicalQuery(req.URL.Query()),
		canonHeaders.String(),
		signed,
		payloadHash,
	}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])

	k := hmacSHA256([]byte("AWS4"+secretKey), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
		", SignedHeaders="+signed+", Signature="+sig)
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func trimAll(vs []string) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = strings.Join(strings.Fields(v), " ")
	}
	return out
}

// uriEncode is SigV4's URI encoding: every byte but A-Z a-z 0-9 - _ . ~ is
// %XX (upper-case hex); "/" is kept unless encodeSlash.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}
