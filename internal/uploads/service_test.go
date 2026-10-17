package uploads_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/uploads"
	"github.com/ZacxDev/muster/internal/uploads/uploadstest"
)

var png = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89rest-of-image")

func as(uid int64) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleMember, Via: authz.ViaSession})
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newService(t *testing.T, max int64) (*uploads.Service, *uploadstest.Store, *uploadstest.Objects, *clock) {
	t.Helper()
	sg, err := uploads.NewSigner("service-test-key-0123456789abcdef-xyz")
	if err != nil {
		t.Fatal(err)
	}
	st, obj := &uploadstest.Store{}, &uploadstest.Objects{}
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	return &uploads.Service{Store: st, Objects: obj, Signer: sg, PublicURL: "https://muster.example.test",
		MaxBytes: max, LinkTTL: time.Hour, Now: c.now}, st, obj, c
}

func linkParts(t *testing.T, link string) (string, url.Values) {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(u.Path, uploads.LinkPath), u.Query()
}

func read(t *testing.T, o uploads.Opened) []byte {
	t.Helper()
	defer o.Body.Close()
	b, err := io.ReadAll(o.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestTheSizeCapIsInclusive: exactly MaxBytes is stored; one more is
// ErrTooLarge and nothing is written anywhere.
func TestTheSizeCapIsInclusive(t *testing.T) {
	svc, st, obj, _ := newService(t, 64)
	ok := bytes.Repeat([]byte("a"), 64)
	if _, err := svc.Put(as(2), ok, "a.txt", "", ""); err != nil {
		t.Fatalf("a file of exactly the cap: %v", err)
	}
	over := bytes.Repeat([]byte("a"), 65)
	if _, err := svc.Put(as(2), over, "b.txt", "", ""); !errors.Is(err, uploads.ErrTooLarge) {
		t.Fatalf("one byte over the cap: %v; want ErrTooLarge", err)
	}
	if n := len(st.Rows()); n != 1 {
		t.Errorf("%d rows; want only the one under the cap", n)
	}
	if n := len(obj.Keys()); n != 1 {
		t.Errorf("%d objects; want only the one under the cap", n)
	}
	if _, err := svc.Put(as(2), nil, "c.txt", "", ""); !errors.Is(err, uploads.ErrEmpty) {
		t.Errorf("an empty file: %v; want ErrEmpty", err)
	}
}

// TestTheTypeIsSniffedAndTheKeyIsRandom: an HTML file uploaded as
// "cute.png" is recorded as text/html (served octet-stream), and neither its
// name nor anything of it is in the object key.
func TestTheTypeIsSniffedAndTheKeyIsRandom(t *testing.T) {
	svc, _, obj, _ := newService(t, 1<<20)
	st, err := svc.Put(as(2), []byte("<html><script>alert(1)</script></html>"), "cute.png", "mirror", "laptop/work/@1")
	if err != nil {
		t.Fatal(err)
	}
	if st.ContentType != "text/html; charset=utf-8" {
		t.Errorf("recorded type %q; want the sniffed text/html", st.ContentType)
	}
	if st.ObjectKey != "uploads/"+st.PublicID || strings.Contains(st.ObjectKey, "cute") {
		t.Errorf("object key %q", st.ObjectKey)
	}
	if ct := obj.Types[st.ObjectKey]; ct != "application/octet-stream" {
		t.Errorf("stored with Content-Type %q; want application/octet-stream", ct)
	}
	if st.Filename != "cute.png" || st.Name != "cute.png" || st.TargetKind != "mirror" || st.Target != "laptop/work/@1" {
		t.Errorf("row = %+v", st.Upload)
	}
	if !strings.Contains(st.Line, st.URL) || !strings.HasPrefix(st.Line, "[attached: cute.png (text/html; charset=utf-8, ") {
		t.Errorf("line = %q", st.Line)
	}
}

// TestALinkOpensUntilItExpiresOrIsRevoked: the link minted at upload serves
// the bytes; tampered, expired or revoked it is refused; revoking deletes the
// object.
func TestALinkOpensUntilItExpiresOrIsRevoked(t *testing.T) {
	svc, _, obj, c := newService(t, 1<<20)
	st, err := svc.Put(as(2), png, "shot.png", "", "")
	if err != nil {
		t.Fatal(err)
	}
	id, q := linkParts(t, st.URL)
	// Any context: the link is the credential (no principal).
	f, err := svc.OpenLink(context.Background(), id, q)
	if err != nil {
		t.Fatalf("OpenLink: %v", err)
	}
	if got := read(t, f); !bytes.Equal(got, png) || f.ContentType != "image/png" {
		t.Errorf("served %d bytes as %q", len(got), f.ContentType)
	}
	bad := url.Values{"exp": q["exp"], "sig": {strings.Repeat("A", 43)}}
	if _, err := svc.OpenLink(context.Background(), id, bad); !uploads.IsRefusal(err) {
		t.Errorf("a forged signature: %v; want a refusal", err)
	}
	c.t = st.ExpiresAt
	if _, err := svc.OpenLink(context.Background(), id, q); !errors.Is(err, uploads.ErrLinkExpired) {
		t.Errorf("at expiry: %v; want ErrLinkExpired", err)
	}
	c.t = st.ExpiresAt.Add(-time.Minute)
	// Another user cannot revoke it.
	if _, err := svc.Revoke(as(3), id); !errors.Is(err, uploads.ErrNotFound) {
		t.Errorf("another user's revoke: %v; want ErrNotFound", err)
	}
	if _, err := svc.OpenLink(context.Background(), id, q); err != nil {
		t.Fatalf("CONTROL: the link stopped working after a refused revoke: %v", err)
	}
	if objErr, err := svc.Revoke(as(2), id); err != nil || objErr != nil {
		t.Fatalf("owner's revoke: %v / %v", err, objErr)
	}
	if _, err := svc.OpenLink(context.Background(), id, q); !errors.Is(err, uploads.ErrNotFound) {
		t.Errorf("after revoke: %v; want ErrNotFound", err)
	}
	if obj.Has(st.ObjectKey) {
		t.Error("the object outlived its revocation")
	}
	if _, err := svc.OpenOwned(as(2), id); !errors.Is(err, uploads.ErrNotFound) {
		t.Errorf("the owner's view after revoke: %v; want ErrNotFound", err)
	}
}

// TestTheOwnersViewIsTheOwnersOnly.
func TestTheOwnersViewIsTheOwnersOnly(t *testing.T) {
	svc, _, _, _ := newService(t, 1<<20)
	st, err := svc.Put(as(2), png, "shot.png", "", "")
	if err != nil {
		t.Fatal(err)
	}
	f, err := svc.OpenOwned(as(2), st.PublicID)
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	f.Body.Close()
	if _, err := svc.OpenOwned(as(3), st.PublicID); !errors.Is(err, uploads.ErrNotFound) {
		t.Errorf("another user: %v; want ErrNotFound", err)
	}
}

// TestAFailedRowTakesItsObjectAway: when the row cannot be written, the
// object already stored is deleted again.
func TestAFailedRowTakesItsObjectAway(t *testing.T) {
	svc, _, obj, _ := newService(t, 1<<20)
	svc.Store = failingStore{svc.Store}
	if _, err := svc.Put(as(2), png, "shot.png", "", ""); err == nil {
		t.Fatal("Put succeeded with a failing store")
	}
	if keys := obj.Keys(); len(keys) != 0 {
		t.Errorf("objects left behind: %v", keys)
	}
}

type failingStore struct{ uploads.Store }

func (failingStore) Create(context.Context, uploads.Upload) (uploads.Upload, error) {
	return uploads.Upload{}, errors.New("db down")
}
