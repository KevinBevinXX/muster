//go:build s3live

package uploads

// 🔴 A BUILD TAG, NOT t.Skip — the reason internal/agentgateway/liveruntime_test.go
// and cmd/muster/liveenv_test.go give: a test that skips without its environment is
// invisible in a green `go test`, and muster's verdict gate (tests/verdict.py)
// refuses a run that skipped anything. The tag states the dependence at compile
// time, and with the tag the test FAILS rather than skips when it has no S3.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"testing"
)

// TestAgainstARealS3 runs the client against a REAL S3-compatible server when
// one is named, and skips otherwise. It is the live control behind the fakes
// above, run by hand (docs/testing.md):
//
//	MUSTER_TEST_S3_ENDPOINT=http://127.0.0.1:9000 MUSTER_TEST_S3_BUCKET=muster-test \
//	  MUSTER_TEST_S3_ACCESS_KEY=… MUSTER_TEST_S3_SECRET_KEY=… \
//	  go test -tags s3live -run TestAgainstARealS3 -v ./internal/uploads/
//
// (any S3-compatible server on loopback; the bucket must exist). It writes a
// small file in one request AND a file over PartSize as a multipart stream of
// unknown length, reads both back, HEADs them, and checks that a wrong secret
// is REFUSED by the server.
func TestAgainstARealS3(t *testing.T) {
	cfg := S3Config{Endpoint: os.Getenv("MUSTER_TEST_S3_ENDPOINT"), Bucket: os.Getenv("MUSTER_TEST_S3_BUCKET"),
		AccessKey: os.Getenv("MUSTER_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("MUSTER_TEST_S3_SECRET_KEY")}
	if cfg.Endpoint == "" {
		t.Fatal("MUSTER_TEST_S3_ENDPOINT is unset: this test was asked for (-tags s3live) and has no S3 to run against")
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
	cov, lerr := c.Lifecycle(ctx)
	t.Logf("Lifecycle on %s: %+v, %v", cfg.Bucket, cov, lerr)

	// A multipart write whose READER fails, its context alive: minio-go aborts
	// it, nothing incomplete is left.
	id, _ := NewPublicID()
	key := ObjectKeyFor(id)
	err = c.Put(ctx, key, &failAfter{n: PartSize + PartSize/2, err: errors.New("client went away")}, -1, "application/octet-stream")
	if err == nil {
		t.Fatal("a Put whose reader failed succeeded")
	}
	if n, err := c.Incomplete(ctx, key); err != nil || n != 0 {
		t.Errorf("after a failed read with a live context: %d incomplete upload(s), %v; want 0", n, err)
	}
	// The same, but the context is cancelled as the reader fails — a request
	// whose client went away: minio-go's own abort cannot go out, and the
	// upload is left incomplete. Cleanup under a fresh context removes it.
	id, _ = NewPublicID()
	key = ObjectKeyFor(id)
	cctx, cancel := context.WithCancel(ctx)
	err = c.Put(cctx, key, &failAfter{n: PartSize + PartSize/2, err: errors.New("client went away"), hook: cancel}, -1, "application/octet-stream")
	cancel()
	if err == nil {
		t.Fatal("a Put whose context was cancelled succeeded")
	}
	orphans, err := c.Incomplete(ctx, key)
	t.Logf("after a cancelled write: %d incomplete upload(s) (%v) — what the request context used to orphan", orphans, err)
	if err := c.Cleanup(ctx, key); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if n, err := c.Incomplete(ctx, key); err != nil || n != 0 {
		t.Errorf("after Cleanup: %d incomplete upload(s), %v; want 0", n, err)
	}
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
