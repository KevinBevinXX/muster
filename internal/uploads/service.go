package uploads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// MaxMaxBytes is the most MUSTER_UPLOAD_MAX_BYTES may say: a file is held
	// whole in memory while it is sniffed, digested and stored.
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

// Put stores one file: data is the WHOLE file (the caller has already bounded
// it by MaxBytes), filename the browser's name for it, target which composer it
// was attached in. It runs under the caller's principal (the row's owner).
//
// The object is written FIRST and the row second; a row that cannot be written
// takes its object away again, so a failure leaves no link and no orphan the
// service knows how to make.
func (s *Service) Put(ctx context.Context, data []byte, filename, targetKind, target string) (Stored, error) {
	if len(data) == 0 {
		return Stored{}, ErrEmpty
	}
	if int64(len(data)) > s.MaxBytes {
		return Stored{}, ErrTooLarge
	}
	id, err := NewPublicID()
	if err != nil {
		return Stored{}, err
	}
	sum := sha256.Sum256(data)
	u := Upload{
		PublicID:    id,
		ObjectKey:   ObjectKeyFor(id),
		Filename:    CleanFilename(filename),
		ContentType: Sniff(data),
		Size:        int64(len(data)),
		SHA256:      hex.EncodeToString(sum[:]),
		TargetKind:  targetKind,
		Target:      target,
		ExpiresAt:   s.now().Add(s.LinkTTL).Truncate(time.Second),
	}
	if err := s.Objects.Put(ctx, u.ObjectKey, data, ServeAs(u.ContentType).ContentType); err != nil {
		return Stored{}, err
	}
	row, err := s.Store.Create(ctx, u)
	if err != nil {
		if derr := s.Objects.Delete(context.WithoutCancel(ctx), u.ObjectKey); derr != nil && s.Logger != nil {
			s.Logger.Printf("uploads: object %s outlived its failed row: %v", u.PublicID, derr)
		}
		return Stored{}, err
	}
	return s.Describe(row), nil
}

// Opened is a file ready to stream.
type Opened struct {
	Upload
	Body io.ReadCloser
	// Size is the object's length as the store reports it (-1 unknown).
	Size int64
}

// OpenLink checks a signed link — the signature, its expiry, and that the
// upload exists and is not revoked — and opens the file. Every refusal is
// ErrNotFound, ErrBadLink, ErrBadSig or ErrLinkExpired; the handler answers
// them all alike.
func (s *Service) OpenLink(ctx context.Context, publicID string, q url.Values) (Opened, error) {
	if err := s.Signer.Verify(publicID, q, s.now()); err != nil {
		return Opened{}, err
	}
	u, err := s.Store.LinkTarget(ctx, publicID)
	if err != nil {
		return Opened{}, err
	}
	if u.Revoked() {
		return Opened{}, ErrNotFound
	}
	return s.open(ctx, u)
}

// OpenOwned opens the caller's own upload (the owner's preview), revoked or
// not until its object is gone.
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
