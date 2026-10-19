package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
)

// Audit round 3 of task routing: a mention reservation whose action never
// completed must not swallow the resend (a cancelled request, a crash); a
// mention and a chief event with the same comment are different rows; and a
// mention is not refused by the chief's undelivered queue.

// cancellingAgents cancels the request's context as the mention's delivery
// opens the agent's conversation — a plugin client that gave up mid-action —
// and fails that call with the context's error.
type cancellingAgents struct {
	agents.Store
	cancel context.CancelFunc
}

func (c cancellingAgents) LatestOrCreateSession(ctx context.Context, id int64, name string) (agents.ChatSession, error) {
	c.cancel()
	<-ctx.Done()
	return agents.ChatSession{}, ctx.Err()
}

// TestAMentionCutOffByACancelledRequestIsDeliveredOnTheResend: the plugin's
// request is cancelled after the mention was reserved and before it was
// delivered; the resend (a new request) delivers it rather than being
// answered as done.
func TestAMentionCutOffByACancelledRequestIsDeliveredOnTheResend(t *testing.T) {
	f := newTaskRouteFixture(t, false, false)
	tok, p := f.plugin(t)
	key := "tracker:cx" + strconv.FormatInt(time.Now().UnixNano(), 36)
	a, turns := f.mentionWorker(t, tok, key)
	ctx, cancel := context.WithCancel(f.asOwner)
	defer cancel()
	real := f.srv.ext.Agents
	f.srv.ext.Agents = cancellingAgents{Store: real, cancel: cancel}
	r := httptest.NewRequest(http.MethodPost, "/plugin/v1/tasks", nil).WithContext(ctx)
	if _, err := f.srv.routeTaskNotice(r, p, *a.NoteID, key, noticeMentioned, "m1: cut off"); err == nil {
		t.Fatal("instrument check FAILED: the cancelled delivery reported success")
	}
	f.srv.ext.Agents = real
	if out := f.notice(t, tok, key, noticeMentioned, "m1: cut off", false); out["routed"] != routedAgent {
		t.Fatalf("the resend: %v; want routed agent", out)
	}
	if got := turns(); got != 1 {
		t.Fatalf("turns after the resend: %d; want 1 (the resend delivered the mention)", got)
	}
}

// TestAStaleUnfinishedMentionReservationIsReclaimed: a reservation left by a
// process that died between reserving and acting (no completion, older than
// plugins.NoticeRouteReclaimAfter) is taken over by the next send, which
// delivers. A fresh unfinished one (in flight elsewhere) is not.
func TestAStaleUnfinishedMentionReservationIsReclaimed(t *testing.T) {
	f := newTaskRouteFixture(t, false, false)
	tok, p := f.plugin(t)
	key := "tracker:st" + strconv.FormatInt(time.Now().UnixNano(), 36)
	a, turns := f.mentionWorker(t, tok, key)
	const comment = "m1: the process died"
	in, err := plugins.NewTaskEventIn(plugins.TaskEvent{Event: noticeMentioned, TaskID: *a.NoteID, Key: key, Change: comment},
		mentionRouteDiscriminator(key, comment))
	if err != nil {
		t.Fatal(err)
	}
	m, created, err := f.plugins.ReserveNoticeRoute(f.asOwner, p.ID, *a.NoteID, in, routedAgent)
	if err != nil || !created {
		t.Fatalf("the dead process's reservation: %v %v", created, err)
	}
	if out := f.notice(t, tok, key, noticeMentioned, comment, false); out["routed"] != routedAgent || turns() != 0 {
		t.Fatalf("a send while the reservation is fresh: %v, %d turn(s); want routed agent and no second delivery", out, turns())
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `DO $$ BEGIN
		SET LOCAL session_replication_role = replica;
		UPDATE plugin_messages SET created_at = now() - interval '1 hour' WHERE id = `+strconv.FormatInt(m.ID, 10)+`;
	END $$`)
	if out := f.notice(t, tok, key, noticeMentioned, comment, false); out["routed"] != routedAgent {
		t.Fatalf("a send after the reservation went stale: %v", out)
	}
	if got := turns(); got != 1 {
		t.Fatalf("turns after the stale reservation was reclaimed: %d; want 1", got)
	}
	if out := f.notice(t, tok, key, noticeMentioned, comment, false); out["routed"] != routedAgent || turns() != 1 {
		t.Fatalf("a send after the reclaimed delivery: %v, %d turns; want answered, not delivered again", out, turns())
	}
	// Completed, it is never reclaimed, however old.
	dbtest.OwnerExec(t, dbtest.DSN(t), `DO $$ BEGIN
		SET LOCAL session_replication_role = replica;
		UPDATE plugin_messages SET created_at = now() - interval '2 hours', routed_claimed_at = now() - interval '2 hours'
		 WHERE id = `+strconv.FormatInt(m.ID, 10)+`;
	END $$`)
	if out := f.notice(t, tok, key, noticeMentioned, comment, false); out["routed"] != routedAgent || turns() != 1 {
		t.Fatalf("a send long after the completed delivery: %v, %d turns; want answered, not delivered again", out, turns())
	}
}

// TestAMentionWithTheSameCommentAsAnUpdateIsDelivered: an update routed to the
// chief and a mention carrying the identical comment are two different
// notices; the mention is delivered (it was refused 409 for good when the two
// shared one id).
func TestAMentionWithTheSameCommentAsAnUpdateIsDelivered(t *testing.T) {
	f := newTaskRouteFixture(t, false, false)
	tok, _ := f.plugin(t)
	key := "tracker:sm" + strconv.FormatInt(time.Now().UnixNano(), 36)
	_, turns := f.mentionWorker(t, tok, key)
	const comment = "HQ task changed: status, then a mention"
	if out := f.notice(t, tok, key, noticeUpdate, comment, false); out["routed"] != routedChief {
		t.Fatalf("the update: %v; want routed chief", out)
	}
	code, out := f.handoff(t, tok, map[string]any{"key": key, "comment": comment, "route_to_chief": true, "notice": noticeMentioned})
	if code != http.StatusOK || out["routed"] != routedAgent || turns() != 1 {
		t.Fatalf("the mention with the same comment: %d %v, %d turn(s); want delivered to the agent", code, out, turns())
	}
}

// TestAFullChiefQueueDoesNotRefuseAMention: mentions never reach the chief, so
// a plugin whose chief has MaxTaskEventsQueued task events undelivered (all
// outside the hour) still has a mention delivered.
func TestAFullChiefQueueDoesNotRefuseAMention(t *testing.T) {
	f := newTaskRouteFixture(t, false, false)
	tok, p := f.plugin(t)
	key := "tracker:fq" + strconv.FormatInt(time.Now().UnixNano(), 36)
	_, turns := f.mentionWorker(t, tok, key)
	filler := f.task(t, "filler")
	dbtest.OwnerExec(t, dbtest.DSN(t), `INSERT INTO plugin_messages (owner_id, plugin_id, external_id, body, task_id, created_at)
		SELECT $1, $2, 'queued-' || g, 'b', $3, now() - interval '2 hours' FROM generate_series(1, $4) g`,
		f.owner, p.ID, filler.ID, plugins.MaxTaskEventsQueued)
	if out := f.notice(t, tok, key, noticeMentioned, "m1: behind a full chief queue", false); out["routed"] != routedAgent || turns() != 1 {
		t.Fatalf("a mention behind a full chief queue: %v, %d turn(s); want delivered", out, turns())
	}
}
