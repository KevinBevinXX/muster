package api

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// TestEditingAMessageThatAnsweredAnAskSaysTheAnswerStands: a queued message
// that answered its session's ask when it was queued is edited. The edit form
// says up front that the answer already given will not change, the Save says
// so again (to the script, and as a form's notice), and the stored answer is
// NOT rewritten — the asker was told the first text, as Cancel already says.
// The control is a message that answered nothing, whose Save is quiet.
func TestEditingAMessageThatAnsweredAnAskSaysTheAnswerStands(t *testing.T) {
	f := newAttnFixture(t, func(ext *Extensions, pool *pgxpool.Pool) { ext.SendQueue = sendqueue.NewPG(pool) })
	sys := authz.System(context.Background())
	t.Cleanup(func() { _, _ = f.pool.Exec(sys, `DELETE FROM send_queue WHERE owner_id = 1 AND mirror_host = 'ws'`) })
	a := f.mirrorAsk(t, "Rotate the key?")
	waited := f.waitAs(attention.Asker{MirrorHost: "ws", MirrorSession: focusSID}, a.ID)
	time.Sleep(200 * time.Millisecond)
	queue := func(text string) int64 {
		t.Helper()
		rd, hdr := form(url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "text": {text}})
		if w := f.call(t, http.MethodPost, "/send-queue", rd, merge(f.operator(), hdr)); w.Code != http.StatusNoContent {
			t.Fatalf("queue: %d %s", w.Code, w.Body)
		}
		var id int64
		if err := f.pool.QueryRow(sys, `SELECT max(id) FROM send_queue WHERE owner_id = 1 AND mirror_host = 'ws'`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	answered := queue("rotate it after the deploy")
	if got := recv(t, waited); !strings.Contains(got, `"answer":"rotate it after the deploy"`) {
		t.Fatalf("fixture: attention_wait returned %s", got)
	}
	xhr := map[string]string{"X-Requested-With": "XMLHttpRequest"}
	edit := func(id int64) string {
		t.Helper()
		if w := f.call(t, http.MethodPost, "/send-queue/"+strconv.FormatInt(id, 10)+"/edit", nil, merge(f.operator(), xhr)); w.Code >= 300 {
			t.Fatalf("Edit %d: %d %s", id, w.Code, w.Body)
		}
		var tok string
		if err := f.pool.QueryRow(sys, `SELECT hold_token FROM send_queue WHERE id = $1`, id).Scan(&tok); err != nil {
			t.Fatal(err)
		}
		return tok
	}
	const asker = "main:1 @ ws"
	upFront := "This message already answered " + asker + "'s question when it was queued, and that answer was delivered: a Save changes what is pasted into the session, not the answer."
	afterSave := "Saved — but this message had already answered " + asker + "'s question, and that answer was delivered as first written: the edit does not change it."

	tok := edit(answered)
	list := f.page(t, "/send-queue?host=ws&session=main&window=%401&edit="+strconv.FormatInt(answered, 10)+"&hold="+tok)
	if !strings.Contains(list, `data-queue-edit-form`) || !strings.Contains(list, html.EscapeString(upFront)) {
		t.Fatalf("the edit form does not say up front that the answer stands: %s", list)
	}
	rd, hdr := form(url.Values{"hold": {tok}, "text": {"rotate it tonight instead"}})
	w := f.call(t, http.MethodPost, "/send-queue/"+strconv.FormatInt(answered, 10)+"/edit/save", rd, merge(merge(f.operator(), hdr), xhr))
	if w.Code != http.StatusOK || w.Body.String() != afterSave {
		t.Fatalf("Save of a message that answered an ask: %d %q; want 200 %q", w.Code, w.Body, afterSave)
	}
	var answer, body string
	if err := f.pool.QueryRow(sys, `SELECT answer FROM attention_asks WHERE id = $1`, a.ID).Scan(&answer); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(sys, `SELECT body FROM send_queue WHERE id = $1`, answered).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if answer != "rotate it after the deploy" || body != "rotate it tonight instead" {
		t.Fatalf("after the Save: the stored answer is %q (want it unchanged), the message %q (want the edit)", answer, body)
	}
	// As a form, the same Save comes back with the notice that says so.
	tok = edit(answered)
	rd, hdr = form(url.Values{"hold": {tok}, "text": {"rotate it at noon"}})
	w = f.call(t, http.MethodPost, "/send-queue/"+strconv.FormatInt(answered, 10)+"/edit/save", rd,
		merge(merge(f.operator(), hdr), map[string]string{"HX-Request": "", "Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate"}))
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "notice=edited-answered") {
		t.Fatalf("a form's Save of a message that answered an ask: %d (Location %q)", w.Code, w.Header().Get("Location"))
	}

	// CONTROL: a message that answered nothing saves quietly.
	plain := queue("and one more thing")
	tok = edit(plain)
	rd, hdr = form(url.Values{"hold": {tok}, "text": {"and one more thing, edited"}})
	if w := f.call(t, http.MethodPost, "/send-queue/"+strconv.FormatInt(plain, 10)+"/edit/save", rd, merge(merge(f.operator(), hdr), xhr)); w.Code != http.StatusNoContent {
		t.Fatalf("CONTROL: a plain Save: %d %q", w.Code, w.Body)
	}
}
