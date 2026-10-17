package uploads

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"time"
)

// Defaults and bounds of the deployment's knobs.
const (
	// DefaultMaxBytes caps one file (MUSTER_UPLOAD_MAX_BYTES).
	DefaultMaxBytes int64 = 25 << 20
	// MaxMaxBytes is the most MUSTER_UPLOAD_MAX_BYTES may say. A file is
	// STREAMED to the store (at most about two PartSize buffers held at once),
	// so this bounds the bucket, not memory.
	MaxMaxBytes int64 = 100 << 20
	// DefaultLinkTTL is how long a link works (MUSTER_UPLOAD_LINK_TTL).
	DefaultLinkTTL = 24 * time.Hour
	// MinLinkTTL and MaxLinkTTL bound MUSTER_UPLOAD_LINK_TTL.
	MinLinkTTL = time.Minute
	MaxLinkTTL = 7 * 24 * time.Hour
)

// Service is the upload feature: a store of rows, a bucket of bytes, and the
// key links are signed with. A nil *Service is "feature off".
type Service struct {
	Store   Store
	Objects ObjectStore
	Signer  Signer
	// PublicURL is MUSTER_PUBLIC_URL: the base every link is written on.
	PublicURL string
	MaxBytes  int64
	LinkTTL   time.Duration
	Logger    *log.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Stored is an upload as the composer is told about it.
type Stored struct {
	Upload
	// Name is the SafeName the agent sees.
	Name string
	// URL is the signed link.
	URL string
	// Line is the text the message carries (AgentLine).
	Line string
}

// Link is the signed URL for u (its own expiry).
func (s *Service) Link(u Upload) string { return s.Signer.URL(s.PublicURL, u.PublicID, u.ExpiresAt) }

// Describe is u as the composer is told about it.
func (s *Service) Describe(u Upload) Stored {
	name := SafeName(u.Filename)
	link := s.Link(u)
	return Stored{Upload: u, Name: name, URL: link, Line: AgentLine(name, u.ContentType, u.Size, link)}
}

// errOverCap is what capReader answers once more than its cap was read.
var errOverCap = errors.New("uploads: over the size cap")

// capReader counts what it reads and fails, with errOverCap, as soon as more
// than max bytes have come through — so a file over the cap aborts the write
// it is streamed into rather than being stored.
type capReader struct {
	r    io.Reader
	n    int64
	max  int64
	over bool
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.over {
		return 0, errOverCap
	}
	if room := c.max + 1 - c.n; int64(len(p)) > room {
		p = p[:room]
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.max {
		c.over = true
		return n, errOverCap
	}
	return n, err
}

// Put stores one file, read from r, under the caller's principal (the row's
// owner). filename is the browser's name for it.
//
// 🔴 THE FILE IS STREAMED, NEVER HELD WHOLE. Its type is sniffed from the
// first SniffBytes; then up to one PartSize is read. A file that ends within
// it is written in one request of known size; a longer one is written as a
// multipart stream (size -1), so at most about two parts are in memory
// whatever MaxBytes says. One byte over MaxBytes fails the read the store is
// consuming, which aborts the write: ErrTooLarge, and nothing is stored.
//
// The object is written FIRST and the row second; a row that cannot be written
// takes its object away again.
func (s *Service) Put(ctx context.Context, r io.Reader, filename string) (Stored, error) {
	br := bufio.NewReaderSize(r, SniffBytes)
	head, perr := br.Peek(SniffBytes)
	if len(head) == 0 {
		if perr != nil && !errors.Is(perr, io.EOF) {
			return Stored{}, perr
		}
		return Stored{}, ErrEmpty
	}
	contentType := Sniff(head)
	id, err := NewPublicID()
	if err != nil {
		return Stored{}, err
	}
	key := ObjectKeyFor(id)
	cr := &capReader{r: br, max: s.MaxBytes}
	first := make([]byte, min(int64(PartSize), s.MaxBytes+1))
	n, rerr := io.ReadFull(cr, first)
	served := ServeAs(contentType).ContentType
	switch {
	case cr.over:
		return Stored{}, ErrTooLarge
	case errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF):
		err = s.Objects.Put(ctx, key, bytes.NewReader(first[:n]), int64(n), served)
	case rerr != nil:
		return Stored{}, rerr
	default:
		err = s.Objects.Put(ctx, key, io.MultiReader(bytes.NewReader(first), cr), -1, served)
	}
	if err != nil {
		if cr.over {
			// Aborted by the cap mid-stream. A store that kept a partial
			// object anyway loses it here.
			_ = s.Objects.Delete(context.WithoutCancel(ctx), key)
			return Stored{}, ErrTooLarge
		}
		return Stored{}, err
	}
	u := Upload{
		PublicID:    id,
		ObjectKey:   key,
		Filename:    CleanFilename(filename),
		ContentType: contentType,
		Size:        cr.n,
		ExpiresAt:   s.now().Add(s.LinkTTL).Truncate(time.Second),
	}
	row, err := s.Store.Create(ctx, u)
	if err != nil {
		if derr := s.Objects.Delete(context.WithoutCancel(ctx), key); derr != nil && s.Logger != nil {
			s.Logger.Printf("uploads: object %s outlived its failed row: %v", id, derr)
		}
		return Stored{}, err
	}
	return s.Describe(row), nil
}

// Opened is a file ready to stream.
type Opened struct {
	Upload
	// Body is nil for a Stat (a HEAD).
	Body io.ReadCloser
	// Size is the object's length as the store reports it.
	Size int64
}

// linkTarget checks a signed link — the signature, its expiry, and that the
// upload exists and is not revoked — and returns its row.
func (s *Service) linkTarget(ctx context.Context, publicID string, q url.Values) (Upload, error) {
	if err := s.Signer.Verify(publicID, q, s.now()); err != nil {
		return Upload{}, err
	}
	u, err := s.Store.LinkTarget(ctx, publicID)
	if err != nil {
		return Upload{}, err
	}
	if u.Revoked() {
		return Upload{}, ErrNotFound
	}
	return u, nil
}

// OpenLink checks a signed link and opens the file. Every refusal is
// ErrNotFound, ErrBadLink, ErrBadSig or ErrLinkExpired; the handler answers
// them all alike.
func (s *Service) OpenLink(ctx context.Context, publicID string, q url.Values) (Opened, error) {
	u, err := s.linkTarget(ctx, publicID, q)
	if err != nil {
		return Opened{}, err
	}
	return s.open(ctx, u)
}

// StatLink checks a signed link exactly as OpenLink does and reports the
// file's size WITHOUT fetching its body (a HEAD).
func (s *Service) StatLink(ctx context.Context, publicID string, q url.Values) (Opened, error) {
	u, err := s.linkTarget(ctx, publicID, q)
	if err != nil {
		return Opened{}, err
	}
	size, err := s.Objects.Stat(ctx, u.ObjectKey)
	if err != nil {
		return Opened{}, err
	}
	return Opened{Upload: u, Size: size}, nil
}

// OpenOwned opens the caller's own upload (the owner's preview), until it is
// revoked.
func (s *Service) OpenOwned(ctx context.Context, publicID string) (Opened, error) {
	u, err := s.Store.Get(ctx, publicID)
	if err != nil {
		return Opened{}, err
	}
	if u.Revoked() {
		return Opened{}, ErrNotFound
	}
	return s.open(ctx, u)
}

func (s *Service) open(ctx context.Context, u Upload) (Opened, error) {
	body, size, err := s.Objects.Get(ctx, u.ObjectKey)
	if err != nil {
		return Opened{}, err
	}
	return Opened{Upload: u, Body: body, Size: size}, nil
}

// Revoke ends the caller's upload's link: the row is marked revoked (so the
// link answers 404 at once) and the object is deleted. err is a failure to
// revoke (ErrNotFound for an upload that is not the caller's); objectErr, with
// err nil, is a revocation that HAPPENED whose object could not be deleted —
// the link is dead either way, and the bytes outlive it until the bucket's
// lifecycle rule takes them.
func (s *Service) Revoke(ctx context.Context, publicID string) (objectErr, err error) {
	u, err := s.Store.Revoke(ctx, publicID)
	if err != nil {
		return nil, err
	}
	if derr := s.Objects.Delete(ctx, u.ObjectKey); derr != nil {
		return fmt.Errorf("uploads: revoked, but the object was not deleted: %w", derr), nil
	}
	return nil, nil
}

// IsRefusal reports whether err is one of the answers a link check gives
// (every one of them is a 404 to the caller).
func IsRefusal(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrBadLink) || errors.Is(err, ErrBadSig) || errors.Is(err, ErrLinkExpired)
}
