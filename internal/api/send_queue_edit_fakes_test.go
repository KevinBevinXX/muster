package api

import (
	"context"
	"time"

	"github.com/ZacxDev/muster/internal/sendqueue"
)

// memQueue's edit methods: the in-memory queue the non-Postgres tests use has
// no holds. The edit routes are tested against the real store
// (send_queue_edit_pg_test.go), whose Claim is what a hold actually gates.

func (m *memQueue) Hold(ctx context.Context, id int64) (sendqueue.Item, error) {
	it, err := m.Get(ctx, id)
	if err != nil {
		return sendqueue.Item{}, err
	}
	return it, sendqueue.ErrNotEditable
}

func (m *memQueue) SaveEdit(ctx context.Context, id int64, _, _ string, _ time.Time) (sendqueue.Item, error) {
	it, err := m.Get(ctx, id)
	if err != nil {
		return sendqueue.Item{}, err
	}
	return it, sendqueue.ErrHoldLapsed
}

func (m *memQueue) ReleaseEdit(ctx context.Context, id int64, _ string) (sendqueue.Item, bool, error) {
	it, err := m.Get(ctx, id)
	return it, false, err
}
