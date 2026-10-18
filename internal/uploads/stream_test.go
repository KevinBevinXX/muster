package uploads_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ZacxDev/muster/internal/uploads"
	"github.com/ZacxDev/muster/internal/uploads/uploadstest"
)

// streamWatch is an object store that records, the moment Put first reads a
// byte, how much of the body its producer had written by then.
type streamWatch struct {
	*uploadstest.Objects
	produced     *atomic.Int64
	atFirstRead  int64
	firstReadSet bool
}

func (s *streamWatch) Put(ctx context.Context, key string, r io.Reader, size int64, ct string) error {
	return s.Objects.Put(ctx, key, readFunc(func(p []byte) (int, error) {
		n, err := r.Read(p)
		if n > 0 && !s.firstReadSet {
			s.firstReadSet, s.atFirstRead = true, s.produced.Load()
		}
		return n, err
	}), size, ct)
}

type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

// producer writes total bytes into a pipe in 64 KiB writes, counting them.
func producer(t *testing.T, total int) (io.Reader, *atomic.Int64) {
	t.Helper()
	pr, pw := io.Pipe()
	var n atomic.Int64
	go func() {
		chunk := bytes.Repeat([]byte("0123456789abcdef"), 4096)
		for left := total; left > 0; {
			k := min(left, len(chunk))
			if _, err := pw.Write(chunk[:k]); err != nil {
				return
			}
			n.Add(int64(k))
			left -= k
		}
		pw.Close()
	}()
	t.Cleanup(func() { pr.CloseWithError(io.ErrClosedPipe) })
	return pr, &n
}

// TestAFileIsStreamedNotHeldWhole: a file of three and a half parts reaches
// the store while most of it is still unwritten by its sender — it is not read
// whole first — and is written as a stream of unknown length. A small file is
// one request of known size.
func TestAFileIsStreamedNotHeldWhole(t *testing.T) {
	svc, _, obj, _ := newService(t, 100<<20)
	total := 3*uploads.PartSize + uploads.PartSize/2
	body, produced := producer(t, total)
	w := &streamWatch{Objects: obj, produced: produced}
	svc.Objects = w
	st, err := svc.Put(as(2), body, "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if st.Size != int64(total) || obj.Sizes[st.ObjectKey] != -1 {
		t.Errorf("stored %d bytes, size told %d; want %d streamed with no length", st.Size, obj.Sizes[st.ObjectKey], total)
	}
	// The service may hold ONE part before the store starts reading; the
	// producer can be at most a pipe write past that.
	if limit := int64(uploads.PartSize + 64<<10); w.atFirstRead > limit {
		t.Errorf("the store first read when %d of %d bytes had been sent; want at most %d — the file was buffered, not streamed",
			w.atFirstRead, total, limit)
	}
	small, err := svc.Put(as(2), rd(png), "small.png")
	if err != nil {
		t.Fatal(err)
	}
	if got := obj.Sizes[small.ObjectKey]; got != int64(len(png)) {
		t.Errorf("a small file was written with size %d; want its length %d in one request", got, len(png))
	}
}

// TestTheCapHoldsAcrossParts: with a cap over one part, a file of exactly the
// cap is stored and one byte more aborts the stream — ErrTooLarge, nothing
// stored.
func TestTheCapHoldsAcrossParts(t *testing.T) {
	max := int64(2*uploads.PartSize + 5)
	svc, _, obj, _ := newService(t, max)
	ok, _ := producer(t, int(max))
	if _, err := svc.Put(as(2), ok, "exact.bin"); err != nil {
		t.Fatalf("exactly the cap: %v", err)
	}
	over, _ := producer(t, int(max)+1)
	if _, err := svc.Put(as(2), over, "over.bin"); !errors.Is(err, uploads.ErrTooLarge) {
		t.Fatalf("one byte over: %v; want ErrTooLarge", err)
	}
	if n := len(obj.Keys()); n != 1 {
		t.Errorf("%d objects stored; want only the one at the cap", n)
	}
}

// TestALinkHEADStatsWithoutFetching: StatLink answers the size from the
// store's metadata and never GETs the body — and refuses exactly as OpenLink.
func TestALinkHEADStatsWithoutFetching(t *testing.T) {
	svc, _, obj, _ := newService(t, 1<<20)
	st, err := svc.Put(as(2), rd(png), "shot.png")
	if err != nil {
		t.Fatal(err)
	}
	id, q := linkParts(t, st.URL)
	f, err := svc.StatLink(context.Background(), id, q)
	if err != nil || f.Size != int64(len(png)) || f.Body != nil || f.ContentType != "image/png" {
		t.Fatalf("StatLink = %+v, %v", f, err)
	}
	if obj.Gets != 0 || obj.Stats != 1 {
		t.Errorf("a HEAD made %d GET(s) and %d stat(s); want 0 and 1", obj.Gets, obj.Stats)
	}
	if _, err := svc.StatLink(context.Background(), id, url.Values{"exp": q["exp"], "sig": {strings.Repeat("A", 43)}}); !uploads.IsRefusal(err) {
		t.Errorf("a forged HEAD: %v; want a refusal", err)
	}
}

// cancelAfter yields n bytes, then cancels its context and fails — a client
// that went away mid-upload, as Go reports it to a handler.
type cancelAfter struct {
	n      int
	cancel context.CancelFunc
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	if c.n <= 0 {
		c.cancel()
		return 0, io.ErrUnexpectedEOF
	}
	k := min(len(p), c.n)
	c.n -= k
	return k, nil
}

// TestAFailedWriteLeavesNoIncompleteUpload: a multipart write that fails —
// its request cancelled as the client went away, or the cap overflowed —
// leaves NO incomplete multipart upload and no object behind.
func TestAFailedWriteLeavesNoIncompleteUpload(t *testing.T) {
	t.Run("client gone, request cancelled", func(t *testing.T) {
		svc, _, obj, _ := newService(t, 100<<20)
		ctx, cancel := context.WithCancel(as(2))
		defer cancel()
		if _, err := svc.Put(ctx, &cancelAfter{n: uploads.PartSize + uploads.PartSize/2, cancel: cancel}, "gone.bin"); err == nil {
			t.Fatal("a Put whose client went away succeeded")
		}
		if inc := obj.Incomplete(); len(inc) != 0 {
			t.Errorf("incomplete uploads left behind: %v", inc)
		}
		if keys := obj.Keys(); len(keys) != 0 {
			t.Errorf("objects left behind: %v", keys)
		}
	})
	t.Run("small file cut off: nothing stored as if whole", func(t *testing.T) {
		svc, rows, obj, _ := newService(t, 100<<20)
		ctx, cancel := context.WithCancel(as(2))
		defer cancel()
		if _, err := svc.Put(ctx, &cancelAfter{n: 1000, cancel: cancel}, "cut.bin"); !errors.Is(err, uploads.ErrTruncated) {
			t.Fatalf("a small file cut off: %v; want ErrTruncated", err)
		}
		if keys := obj.Keys(); len(keys) != 0 || len(rows.Rows()) != 0 {
			t.Errorf("a cut-off file was stored: objects %v, rows %d", keys, len(rows.Rows()))
		}
	})
	t.Run("cap overflow mid-stream", func(t *testing.T) {
		svc, _, obj, _ := newService(t, int64(2*uploads.PartSize+5))
		body, _ := producer(t, 3*uploads.PartSize)
		if _, err := svc.Put(as(2), body, "over.bin"); !errors.Is(err, uploads.ErrTooLarge) {
			t.Fatalf("over the cap: %v; want ErrTooLarge", err)
		}
		if inc := obj.Incomplete(); len(inc) != 0 {
			t.Errorf("incomplete uploads left behind: %v", inc)
		}
	})
	// The two layers, each pinned alone.
	t.Run("the write outlives its request's cancellation (cleanup failing)", func(t *testing.T) {
		svc, _, obj, _ := newService(t, 100<<20)
		obj.FailCleanup = errors.New("store unreachable")
		ctx, cancel := context.WithCancel(as(2))
		defer cancel()
		_, _ = svc.Put(ctx, &cancelAfter{n: uploads.PartSize + uploads.PartSize/2, cancel: cancel}, "gone.bin")
		if inc := obj.Incomplete(); len(inc) != 0 {
			t.Errorf("with clean-up failing, the write's own abort did not go out: %v", inc)
		}
	})
	t.Run("a failed write is cleaned up when the store's own abort did not happen", func(t *testing.T) {
		svc, _, obj, _ := newService(t, 100<<20)
		obj.SkipAbort = true
		ctx, cancel := context.WithCancel(as(2))
		defer cancel()
		_, _ = svc.Put(ctx, &cancelAfter{n: uploads.PartSize + uploads.PartSize/2, cancel: cancel}, "gone.bin")
		if inc := obj.Incomplete(); len(inc) != 0 {
			t.Errorf("the explicit clean-up did not remove the incomplete upload: %v", inc)
		}
	})
	t.Run("CONTROL: the fake does orphan a cancelled write", func(t *testing.T) {
		obj := &uploadstest.Objects{}
		ctx, cancel := context.WithCancel(context.Background())
		_ = obj.Put(ctx, "uploads/x", &cancelAfter{n: uploads.PartSize + 10, cancel: cancel}, -1, "x")
		if len(obj.Incomplete()) != 1 {
			t.Fatal("the fake did not keep an upload whose abort's context was cancelled; the tests above prove nothing")
		}
	})
}
