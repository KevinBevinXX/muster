package hermeshost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
)

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestBreadcrumbNameIsHermesSanitisedTTY(t *testing.T) {
	// Literal expectations from P:hermes_cli/terminal_breadcrumbs.py _sanitize:
	// strip, strip '/', [^A-Za-z0-9._-] -> '-', cap 120.
	for tty, want := range map[string]string{
		"/dev/pts/3":   "tty-dev-pts-3",
		"/dev/pts/120": "tty-dev-pts-120",
		"/dev/tty1":    "tty-dev-tty1",
		"/dev/a b/c":   "tty-dev-a-b-c",
		"pts/3":        "", // not a device path
		"":             "",
	} {
		if got := BreadcrumbName(tty); got != want {
			t.Errorf("BreadcrumbName(%q) = %q, want %q", tty, got, want)
		}
	}
}

func TestStateReadsThePinnedSchema(t *testing.T) {
	h := hermestest.New(t)
	root := hermestest.LocalID()
	h.Session(root, hermestest.SessionOpts{})
	h.Message(root, hermestest.Msg{Role: "user", Content: "hello"})
	st := NewHome(h.Dir).OpenState()
	defer st.Close()
	snap, err := st.Snapshot(ctx(t), root)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Lineage.IDs(); !reflect.DeepEqual(got, []string{root}) {
		t.Fatalf("lineage = %v", got)
	}
	if snap.NewestMessage <= 0 || snap.LeaseExpires != 0 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if _, err := st.Snapshot(ctx(t), "local-nope"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("missing session: %v, want ErrNoSession", err)
	}
}

func TestStateRefusesASchemaItCannotRead(t *testing.T) {
	h := hermestest.New(t)
	// The pre-pin shape: no transcript_revision (PIN-ONLY), nor its trigger.
	h.Exec(`DROP TRIGGER messages_transcript_revision_update`)
	h.Exec(`ALTER TABLE sessions DROP COLUMN transcript_revision`)
	st := NewHome(h.Dir).OpenState()
	defer st.Close()
	_, err := st.Snapshot(ctx(t), "x")
	if !errors.Is(err, ErrSchema) {
		t.Fatalf("err = %v, want ErrSchema", err)
	}
}

func TestStateNeverCreatesADatabase(t *testing.T) {
	dir := t.TempDir()
	st := NewHome(dir).OpenState()
	defer st.Close()
	if _, err := st.Snapshot(ctx(t), "x"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want not-exist", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a read created state.db: %v", err)
	}
}

// The reader opens state.db read-only: a write through its connection fails.
func TestStateConnectionIsReadOnly(t *testing.T) {
	h := hermestest.New(t)
	st := NewHome(h.Dir).OpenState()
	defer st.Close()
	db, err := st.conn()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (99)`); err == nil {
		t.Fatal("a write through the reader's connection succeeded")
	}
}

// It reads rows the writer committed to the WAL and has not checkpointed —
// what immutable=1 would miss.
func TestStateSeesUncheckpointedWALWrites(t *testing.T) {
	h := hermestest.New(t)
	h.Exec(`PRAGMA wal_autocheckpoint=0`)
	st := NewHome(h.Dir).OpenState()
	defer st.Close()
	root := hermestest.LocalID()
	if _, err := st.Snapshot(ctx(t), root); !errors.Is(err, ErrNoSession) {
		t.Fatalf("before: %v", err)
	}
	h.Session(root, hermestest.SessionOpts{})
	if fi, err := os.Stat(filepath.Join(h.Dir, "state.db-wal")); err != nil || fi.Size() == 0 {
		t.Fatalf("the write is not in the WAL: %v", err)
	}
	if _, err := st.Snapshot(ctx(t), root); err != nil {
		t.Fatalf("after a WAL-only write: %v", err)
	}
}

func TestLineageFollowsCompressionOnly(t *testing.T) {
	h := hermestest.New(t)
	root, child, grand := hermestest.LocalID(), hermestest.ChildID(), hermestest.ChildID()
	t0 := hermestest.Now()
	h.Session(root, hermestest.SessionOpts{StartedAt: t0})
	h.End(root, "compression")
	h.Session(child, hermestest.SessionOpts{Parent: root, StartedAt: t0 + 1})
	h.End(child, "compression")
	h.Session(grand, hermestest.SessionOpts{Parent: child, StartedAt: t0 + 2})
	// A /branch of the tip shares the parent link; its parent did not end by
	// compression, so it is no continuation.
	branch := hermestest.LocalID()
	h.Session(branch, hermestest.SessionOpts{Parent: grand, StartedAt: t0 + 3})

	st := NewHome(h.Dir).OpenState()
	defer st.Close()
	for _, from := range []string{root, child, grand} {
		snap, err := st.Snapshot(ctx(t), from)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := snap.Lineage.IDs(), []string{root, child, grand}; !reflect.DeepEqual(got, want) {
			t.Errorf("lineage from %s = %v, want %v", from[:10], got, want)
		}
	}
	snap, err := st.Snapshot(ctx(t), branch)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Lineage.IDs(); !reflect.DeepEqual(got, []string{branch}) {
		t.Errorf("a branch's lineage = %v, want only itself", got)
	}
}

// 🔴 A child of a compression-ended parent that is MARKED a branch (or a
// delegation, a reset, a tool session) is not its continuation either.
func TestLineageSkipsMarkedForksOfACompressedParent(t *testing.T) {
	for _, o := range []hermestest.SessionOpts{
		{ModelConfig: `{"_branched_from": "x"}`},
		{ModelConfig: `{"_delegate_from": "x"}`},
		{ModelConfig: `{"_reset_from": "x"}`},
		{Source: "tool"},
	} {
		h := hermestest.New(t)
		root, fork := hermestest.LocalID(), hermestest.ChildID()
		h.Session(root, hermestest.SessionOpts{})
		h.End(root, "compression")
		o.Parent = root
		h.Session(fork, o)
		st := NewHome(h.Dir).OpenState()
		snap, err := st.Snapshot(ctx(t), root)
		st.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := snap.Lineage.IDs(); !reflect.DeepEqual(got, []string{root}) {
			t.Errorf("%+v: lineage = %v, want only the root", o, got)
		}
	}
}

func TestSnapshotReadsTheRootsLease(t *testing.T) {
	h := hermestest.New(t)
	root, child := hermestest.LocalID(), hermestest.ChildID()
	h.Session(root, hermestest.SessionOpts{})
	h.End(root, "compression")
	h.Session(child, hermestest.SessionOpts{Parent: root})
	// The lease is keyed by the logical root, also after a rotation (S2, S4).
	h.Lease(root, hermestest.Now()+300)
	h.Lease(child, hermestest.Now()-1) // a decoy under the child's id
	st := NewHome(h.Dir).OpenState()
	defer st.Close()
	snap, err := st.Snapshot(ctx(t), child)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.LeaseLive(hermestest.Now()) {
		t.Fatalf("the root's live lease was not read: %+v", snap)
	}
}
