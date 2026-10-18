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
	"errors"
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
	// Coverage and RulesErr are what Lifecycle answers.
	Coverage uploads.LifecycleCoverage
	RulesErr error
	// SkipAbort makes a failed multipart Put leave its upload behind even with
	// a live context (the store's own abort request failed); FailCleanup is
	// what Cleanup answers. Each lets a test pin ONE of the two layers that
	// remove a failed write.
	SkipAbort   bool
	FailCleanup error
	// incomplete counts the initiated, unfinished multipart uploads per key.
	incomplete map[string]int
	// FailPut, when set, is what Put answers.
	FailPut error
}

var _ uploads.ObjectStore = (*Objects)(nil)

// AbortLatency is how long the fake's AbortMultipartUpload "round trip"
// takes. minio-go aborts a failed multipart write under the SAME context the
// write ran under; if that context is cancelled meanwhile — a request whose
// client went away — the abort never arrives and the parts stay.
var AbortLatency = 20 * time.Millisecond

// Put stores what it reads from r. A known size is ONE request; an unknown
// size (-1) is a MULTIPART upload, modelled as a real store keeps one: it is
// initiated, each uploads.PartSize part is sent under ctx, and it stays listed
// (Incomplete) until it is completed or aborted. A failure aborts it only if
// ctx survives the abort's round trip.
func (o *Objects) Put(ctx context.Context, key string, r io.Reader, size int64, ct string) error {
	if o.FailPut != nil {
		return o.FailPut
	}
	if size >= 0 {
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		if int64(len(data)) != size {
			return io.ErrUnexpectedEOF
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		o.store(key, data, ct, size)
		return nil
	}
	o.mu.Lock()
	if o.incomplete == nil {
		o.incomplete = map[string]int{}
	}
	o.incomplete[key]++
	o.mu.Unlock()
	var data []byte
	buf := make([]byte, uploads.PartSize)
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if cerr := ctx.Err(); cerr != nil {
				return o.fail(ctx, key, cerr)
			}
			data = append(data, buf[:n]...)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return o.fail(ctx, key, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return o.fail(ctx, key, err)
	}
	o.mu.Lock()
	o.finish(key)
	o.mu.Unlock()
	o.store(key, data, ct, -1)
	return nil
}

// fail is minio-go's deferred abort: sent under ctx, after a round trip.
func (o *Objects) fail(ctx context.Context, key string, err error) error {
	select {
	case <-ctx.Done():
	case <-time.After(AbortLatency):
		if ctx.Err() == nil && !o.SkipAbort {
			o.mu.Lock()
			o.finish(key)
			o.mu.Unlock()
		}
	}
	return err
}

func (o *Objects) finish(key string) {
	if o.incomplete[key]--; o.incomplete[key] <= 0 {
		delete(o.incomplete, key)
	}
}

func (o *Objects) store(key string, data []byte, ct string, size int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.objs == nil {
		o.objs, o.Types, o.Sizes = map[string][]byte{}, map[string]string{}, map[string]int64{}
	}
	o.objs[key] = data
	o.Types[key] = ct
	o.Sizes[key] = size
}

// Incomplete lists the keys with a multipart upload initiated and neither
// completed nor aborted — what a failed write orphans.
func (o *Objects) Incomplete() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []string
	for k := range o.incomplete {
		out = append(out, k)
	}
	return out
}

// Cleanup aborts key's incomplete uploads and deletes key, under ctx.
func (o *Objects) Cleanup(ctx context.Context, key string) error {
	if o.FailCleanup != nil {
		return o.FailCleanup
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.incomplete, key)
	delete(o.objs, key)
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

// Lifecycle answers Coverage, or RulesErr.
func (o *Objects) Lifecycle(context.Context) (uploads.LifecycleCoverage, error) {
	return o.Coverage, o.RulesErr
}

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
