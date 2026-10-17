// Package uploadstest is in-memory fakes of internal/uploads' two ports — the
// row store and the object store — for tests that drive the upload routes
// without Postgres or an S3 server. The Postgres store's own behaviour
// (row-level security included) is tested against Postgres in
// internal/uploads; these fakes model only its OWNER SCOPING, from the
// context's principal, so a route test can tell one user from another.
package uploadstest

import (
	"bytes"
	"context"
	"io"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/uploads"
)

// Store is uploads.Store in memory.
type Store struct {
	mu   sync.Mutex
	rows map[string]uploads.Upload
	next int64
}

var _ uploads.Store = (*Store)(nil)

func owner(ctx context.Context) int64 {
	p, ok := authz.From(ctx)
	if !ok {
		return -1
	}
	return p.UserID
}

// Create stores u as the context principal's.
func (s *Store) Create(ctx context.Context, u uploads.Upload) (uploads.Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[string]uploads.Upload{}
	}
	s.next++
	u.ID, u.OwnerID, u.CreatedAt, u.RevokedAt = s.next, owner(ctx), time.Now(), nil
	s.rows[u.PublicID] = u
	return u, nil
}

// Get is the principal's row.
func (s *Store) Get(ctx context.Context, id string) (uploads.Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.rows[id]
	if !ok || u.OwnerID != owner(ctx) {
		return uploads.Upload{}, uploads.ErrNotFound
	}
	return u, nil
}

// Revoke marks the principal's row revoked.
func (s *Store) Revoke(ctx context.Context, id string) (uploads.Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.rows[id]
	if !ok || u.OwnerID != owner(ctx) {
		return uploads.Upload{}, uploads.ErrNotFound
	}
	if u.RevokedAt == nil {
		now := time.Now()
		u.RevokedAt = &now
		s.rows[id] = u
	}
	return u, nil
}

// LinkTarget is any owner's row.
func (s *Store) LinkTarget(_ context.Context, id string) (uploads.Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.rows[id]
	if !ok {
		return uploads.Upload{}, uploads.ErrNotFound
	}
	return u, nil
}

// Rows is every stored row (a copy).
func (s *Store) Rows() []uploads.Upload {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []uploads.Upload
	for _, u := range s.rows {
		out = append(out, u)
	}
	return out
}

// Objects is uploads.ObjectStore in memory.
type Objects struct {
	mu   sync.Mutex
	objs map[string][]byte
	// Types is the Content-Type each object was stored with, and Sizes the
	// size Put was told (-1: streamed with no length).
	Types map[string]string
	Sizes map[string]int64
	// Gets and Stats count the calls of each.
	Gets, Stats int
	// Rules and RulesErr are what ExpiryRules answers.
	Rules    int
	RulesErr error
	// FailPut, when set, is what Put answers.
	FailPut error
}

var _ uploads.ObjectStore = (*Objects)(nil)

// Put reads r to its end and stores what it read. A read error aborts it
// (nothing is stored), as a real store aborts the write.
func (o *Objects) Put(_ context.Context, key string, r io.Reader, size int64, ct string) error {
	if o.FailPut != nil {
		return o.FailPut
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if size >= 0 && int64(len(data)) != size {
		return io.ErrUnexpectedEOF
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.objs == nil {
		o.objs, o.Types, o.Sizes = map[string][]byte{}, map[string]string{}, map[string]int64{}
	}
	o.objs[key] = data
	o.Types[key] = ct
	o.Sizes[key] = size
	return nil
}

// Stat is a stored object's length, without reading it.
func (o *Objects) Stat(_ context.Context, key string) (int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Stats++
	b, ok := o.objs[key]
	if !ok {
		return 0, uploads.ErrNotFound
	}
	return int64(len(b)), nil
}

// ExpiryRules answers Rules, or RulesErr.
func (o *Objects) ExpiryRules(context.Context) (int, error) { return o.Rules, o.RulesErr }

// Get opens a stored object.
func (o *Objects) Get(_ context.Context, key string) (io.ReadCloser, int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Gets++
	b, ok := o.objs[key]
	if !ok {
		return nil, 0, uploads.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

// Delete removes an object.
func (o *Objects) Delete(_ context.Context, key string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.objs, key)
	return nil
}

// Check always answers.
func (o *Objects) Check(context.Context) error { return nil }

// Keys is every stored key.
func (o *Objects) Keys() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []string
	for k := range o.objs {
		out = append(out, k)
	}
	return out
}

// Has reports whether key is stored.
func (o *Objects) Has(key string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.objs[key]
	return ok
}
