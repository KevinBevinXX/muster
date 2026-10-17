package uploads

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
)

// ---------------------------------------------------------------------------
// THE OBJECT STORE: minio-go, path-style, behind the small ObjectStore
// interface the service and the route tests use (uploadstest.Objects fakes it
// in process; TestAgainstARealS3 runs this against a real server).
//
// 🔴 NO TOTAL TIMEOUT ON THE HTTP CLIENT. A client Timeout covers the response
// BODY too, so a file streamed to a slow agent through GET /f/{id} would be cut
// off mid-file after its 200 had gone out. Every call is bounded by its
// CONTEXT instead (the request's own, for a download: it ends when the agent
// goes away), and the transport bounds only the dial and the wait for response
// HEADERS (S3Config.HeaderTimeout).
// ---------------------------------------------------------------------------

// ObjectStore is what the upload service needs of a bucket.
type ObjectStore interface {
	// Put stores r at key. size is r's length, or -1 when unknown (the file is
	// then streamed in parts). A read error from r aborts the write.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// Get opens key. The caller closes the body. A missing key is ErrNotFound.
	Get(ctx context.Context, key string) (body io.ReadCloser, size int64, err error)
	// Stat is key's size, without its body. A missing key is ErrNotFound.
	Stat(ctx context.Context, key string) (size int64, err error)
	Delete(ctx context.Context, key string) error
	// Check reaches the bucket with the configured credential.
	Check(ctx context.Context) error
	// ExpiryRules counts the bucket's enabled lifecycle rules that expire
	// objects under the uploads/ prefix. ErrLifecycleUnreadable when the
	// credential may not read the configuration.
	ExpiryRules(ctx context.Context) (int, error)
}

// ErrLifecycleUnreadable is a lifecycle configuration the credential may not
// read (s3:GetLifecycleConfiguration is not granted).
var ErrLifecycleUnreadable = errors.New("uploads: the bucket's lifecycle configuration cannot be read with this credential")

// S3Config is an S3-compatible endpoint, bucket and credential.
type S3Config struct {
	// Endpoint is the server's base URL, http(s)://host[:port], no path.
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	// Region is the signing region; MinIO's default is us-east-1.
	Region string
	// HeaderTimeout bounds the wait for a response's headers; 0 is
	// DefaultHeaderTimeout. Nothing bounds a body but its context.
	HeaderTimeout time.Duration
}

// DefaultRegion is the signing region when none is configured.
const DefaultRegion = "us-east-1"

// DefaultHeaderTimeout is how long the store has to start answering.
const DefaultHeaderTimeout = 30 * time.Second

// PartSize is the size of each part of a streamed upload, and so about the
// most one upload holds in memory at once: minio-go buffers one part, and
// Service.Put buffers one more while it decides whether the file fits in a
// single request.
const PartSize = 5 << 20

// S3 is ObjectStore over minio-go.
type S3 struct {
	cfg S3Config
	mc  *minio.Client
}

// NewS3 validates cfg and returns a client.
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
	if cfg.HeaderTimeout <= 0 {
		cfg.HeaderTimeout = DefaultHeaderTimeout
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: cfg.HeaderTimeout,
		ExpectContinueTimeout: time.Second,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
	}
	mc, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       u.Scheme == "https",
		Region:       cfg.Region, // set, so there is no bucket-location round trip
		BucketLookup: minio.BucketLookupPath,
		Transport:    tr,
		// A store that does not answer is tried three times, not minio-go's
		// default ten: each try already waits out HeaderTimeout.
		MaxRetries: 3,
	})
	if err != nil {
		return nil, fmt.Errorf("uploads: S3 client: %w", err)
	}
	return &S3{cfg: cfg, mc: mc}, nil
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

// S3Error is a non-success answer from the store, naming its status and S3's
// code, never a credential.
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

// mapErr turns a minio-go error into ErrNotFound, an *S3Error, or the
// transport error it was.
func mapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	er := minio.ToErrorResponse(err)
	switch {
	case er.Code == "NoSuchKey" || er.Code == "NoSuchObject":
		return ErrNotFound
	case er.StatusCode != 0:
		return &S3Error{Op: op, Status: er.StatusCode, Code: er.Code}
	}
	return fmt.Errorf("uploads: S3 %s: %w", op, err)
}

// Put stores r at key: one request when size is known and within a part,
// otherwise a multipart upload in PartSize pieces (aborted on a read error).
func (c *S3) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	_, err := c.mc.PutObject(ctx, c.cfg.Bucket, key, r, size, minio.PutObjectOptions{
		ContentType: contentType,
		PartSize:    PartSize,
	})
	return mapErr("put", err)
}

// Get opens key: the GET is made, and its status known, before this returns.
func (c *S3) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	obj, err := c.mc.GetObject(ctx, c.cfg.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, mapErr("get", err)
	}
	st, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, 0, mapErr("get", err)
	}
	return obj, st.Size, nil
}

// Stat is HEAD on key.
func (c *S3) Stat(ctx context.Context, key string) (int64, error) {
	st, err := c.mc.StatObject(ctx, c.cfg.Bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return 0, mapErr("stat", err)
	}
	return st.Size, nil
}

// Delete removes key. A key already gone is not an error.
func (c *S3) Delete(ctx context.Context, key string) error {
	err := mapErr("delete", c.mc.RemoveObject(ctx, c.cfg.Bucket, key, minio.RemoveObjectOptions{}))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Check is HEAD on the bucket.
func (c *S3) Check(ctx context.Context) error {
	ok, err := c.mc.BucketExists(ctx, c.cfg.Bucket)
	if err != nil {
		return mapErr("head bucket", err)
	}
	if !ok {
		return &S3Error{Op: "head bucket", Status: http.StatusNotFound, Code: "NoSuchBucket"}
	}
	return nil
}

// ExpiryRules reads the bucket's lifecycle and counts the enabled rules with
// an expiration that apply to the uploads/ prefix.
func (c *S3) ExpiryRules(ctx context.Context) (int, error) {
	cfg, err := c.mc.GetBucketLifecycle(ctx, c.cfg.Bucket)
	if err != nil {
		switch minio.ToErrorResponse(err).Code {
		case "NoSuchLifecycleConfiguration":
			return 0, nil
		case "AccessDenied":
			return 0, ErrLifecycleUnreadable
		}
		return 0, mapErr("get lifecycle", err)
	}
	return CountExpiryRules(cfg), nil
}

// CountExpiryRules counts cfg's enabled rules with an expiration whose prefix
// (none, or one the uploads/ keys start with) covers the uploads/ objects.
func CountExpiryRules(cfg *lifecycle.Configuration) int {
	if cfg == nil {
		return 0
	}
	n := 0
	for _, r := range cfg.Rules {
		if r.Status != "Enabled" || r.Expiration.IsNull() {
			continue
		}
		prefix := r.Prefix
		if r.RuleFilter.Prefix != "" {
			prefix = r.RuleFilter.Prefix
		} else if r.RuleFilter.And.Prefix != "" {
			prefix = r.RuleFilter.And.Prefix
		}
		if strings.HasPrefix(objectKeyPrefix, prefix) {
			n++
		}
	}
	return n
}
