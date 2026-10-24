package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/suggest"
	"github.com/ZacxDev/muster/internal/ui"
)

// SUGGESTED REPLIES, P2's server half: the rendered view's socket drives the
// trigger and tells the page; a turn end is judged by the turn's OWN status
// token (Msg.StatusSince); the cutoff; the chip row's reports and the
// auto-pause; the toggle's Account form.

// setList sets what the fixture's window list says: status st at token since.
func (f *suggestFixture) setList(st string, since int64) {
	f.fm.mu.Lock()
	f.fm.agentState.Status, f.fm.agentState.Since = st, since
	f.fm.mu.Unlock()
}

// readSuggest reads socket messages until a {"type":"suggest"} one, and
// returns its state (and ms).
func readSuggest(t *testing.T, c *websocket.Conn) (string, float64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("no suggest message: %v", err)
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil || m["type"] != "suggest" {
			continue
		}
		ms, _ := m["ms"].(float64)
		return m["state"].(string), ms
	}
}

// TestTheViewSocketSuggestsAtATurnEndAndTellsThePage: the rendered view's
// socket is wired to the trigger (P1 left it unwired): a view opened on an
// idle session that handed control back makes ONE call and tells the page
// pending, then ready — and the page's GET then serves the chip.
func TestTheViewSocketSuggestsAtATurnEndAndTellsThePage(t *testing.T) {
	f := newSuggestFixture(t, nil)
	c, _, err := dialTerm(t, f.s, f.ts, ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}.ViewWSPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if st, ms := readSuggest(t, c); st != "pending" || ms < 0 || ms >= float64(ui.SuggestCutoff.Milliseconds()) {
		t.Fatalf("first: %q %v", st, ms)
	}
	if st, _ := readSuggest(t, c); st != "ready" {
		t.Fatalf("then: %q", st)
	}
	if f.gen.n() != 1 {
		t.Fatalf("calls %d", f.gen.n())
	}
	if v := f.get(t); len(v.Suggestions) != 1 || !v.AI {
		t.Fatalf("served %+v", v)
	}
}

// TestTheViewSocketSaysNoneWhenThereIsNone: a window waiting at a permission
// prompt — the page is told "none" and no call is made.
func TestTheViewSocketSaysNoneWhenThereIsNone(t *testing.T) {
	f := newSuggestFixture(t, func(fm *fakeMirror, _ *Extensions) { fm.agentState.Status = mirror.StatusWaiting })
	c, _, err := dialTerm(t, f.s, f.ts, ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}.ViewWSPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if st, _ := readSuggest(t, c); st != "none" || f.gen.n() != 0 {
		t.Fatalf("waiting: %q, calls %d", st, f.gen.n())
	}
}

const (
	tokBefore  = int64(1_792_000_000_000) // the idle before the turn
	tokBusy1   = int64(1_792_000_001_000)
	tokBetween = int64(1_792_000_002_000) // turn 1's own idle
	tokBusy2   = int64(1_792_000_003_000)
	tokOwn     = int64(1_792_000_004_000) // the (last) turn's own idle
)

const p2Reply = "Done: (a) checkpoint every 500 rows is in. Should I open the PR?"

// TestABackedUpSocketJudgesTheTurnByItsOwnToken (#254's first open trigger
// issue): the socket is backed up, so by the time the turn's busy marker
// reaches the tracker the window list ALREADY reads the turn's own idle. P1
// stamped that as "the idle from before the turn" and then waited for a
// different one that never came: no chips. The turn end now carries its own
// token and is judged by it — one call, at once, served.
func TestABackedUpSocketJudgesTheTurnByItsOwnToken(t *testing.T) {
	f := newSuggestFixture(t, nil)
	f.s.suggestIdleWaitFor = 2 * time.Second
	f.setList(mirror.StatusIdle, tokOwn) // the turn is already over
	tr := &suggestTracker{}
	for _, m := range suggestBackfill() {
		m.StatusSince = tokBefore
		tr.observe(m)
	}
	tr.observe(mirror.Msg{Events: []cctranscript.Event{{Kind: cctranscript.KindPrompt, Text: "go with (a)"}}})
	tr.observe(mirror.Msg{Busy: true, StatusSince: tokBusy1}) // arrives late
	if !tr.observe(mirror.Msg{StatusSince: tokOwn, Events: []cctranscript.Event{{Kind: cctranscript.KindText, Text: p2Reply}}}) {
		t.Fatal("the turn did not end")
	}
	start := time.Now()
	if !f.s.suggestTurn(operatorCtx(), "ws", "main", "@1", tr) || f.gen.n() != 1 {
		t.Fatalf("no set for the turn: calls %d", f.gen.n())
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("waited %v for a status the list already had", d)
	}
	if v := f.get(t); len(v.Suggestions) != 1 || !strings.Contains(f.gen.prompts[0], "Should I open the PR?") {
		t.Fatalf("served %+v", v)
	}
}

// TestBackToBackTurnsAreJudgedByTheLastTurnsOwnToken (#254's second open
// trigger issue): two turns back to back with no idle on the stream between
// them. The list still shows turn 1's idle when turn 2 ends; P1 took that for
// turn 2's own (it differs from the idle before turn 1) and stamped the set
// with it, so the moment the list caught up the set stopped being served. The
// turn end now waits for turn 2's own token and the set answers it.
func TestBackToBackTurnsAreJudgedByTheLastTurnsOwnToken(t *testing.T) {
	f := newSuggestFixture(t, nil)
	f.s.suggestIdleWaitFor = 2 * time.Second
	f.setList(mirror.StatusIdle, tokBefore)
	tr := &suggestTracker{}
	for _, m := range suggestBackfill() {
		m.StatusSince = tokBefore
		tr.observe(m)
	}
	tr.observe(mirror.Msg{Events: []cctranscript.Event{{Kind: cctranscript.KindPrompt, Text: "go with (a)"}}})
	tr.observe(mirror.Msg{Busy: true, StatusSince: tokBusy1})
	f.setList(mirror.StatusIdle, tokBetween) // turn 1 ended; the stream never showed it
	if tr.observe(mirror.Msg{Busy: true, StatusSince: tokBusy2}) {
		t.Fatal("a busy status ended a turn")
	}
	if !tr.observe(mirror.Msg{StatusSince: tokOwn, Events: []cctranscript.Event{{Kind: cctranscript.KindText, Text: p2Reply}}}) {
		t.Fatal("turn 2 did not end")
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		f.setList(mirror.StatusIdle, tokOwn) // turn 2's own idle reaches the list
	}()
	if !f.s.suggestTurn(operatorCtx(), "ws", "main", "@1", tr) || f.gen.n() != 1 {
		t.Fatalf("calls %d", f.gen.n())
	}
	f.setList(mirror.StatusIdle, tokOwn)
	if v := f.get(t); len(v.Suggestions) != 1 {
		t.Fatalf("turn 2's set is not served once the list caught up: %+v", v)
	}
}

// TestALaterStatusThanTheTurnsOwnEndsTheWaitAtOnce: the list already reads a
// status AFTER the turn's own (the window moved on): no call, and no wait —
// that later status's own marker ends its own turn.
func TestALaterStatusThanTheTurnsOwnEndsTheWaitAtOnce(t *testing.T) {
	f := newSuggestFixture(t, nil)
	f.s.suggestIdleWaitFor = 2 * time.Second
	f.setList(mirror.StatusIdle, tokOwn+5000)
	tr := &suggestTracker{}
	for _, m := range suggestBackfill() {
		m.StatusSince = tokOwn
		tr.observe(m)
	}
	start := time.Now()
	if f.s.suggestTurn(operatorCtx(), "ws", "main", "@1", tr) || f.gen.n() != 0 {
		t.Fatalf("a set for a turn the window moved past: calls %d", f.gen.n())
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("waited %v for a token that can no longer come", d)
	}
}

// TestAPromptFromBeforeTheTurnEndIsWaitedOutAndItsOwnIsNot: a WAITING status
// older than the turn's own token is the list lagging (waited out, then the
// set is made); a waiting status at or after it is a real prompt (no call, at
// once).
func TestAPromptFromBeforeTheTurnEndIsWaitedOutAndItsOwnIsNot(t *testing.T) {
	for _, c := range []struct {
		name  string
		since int64
		want  bool
	}{{"stale prompt", tokBusy1, true}, {"its own prompt (the turn ended AT it)", tokOwn, false}} {
		t.Run(c.name, func(t *testing.T) {
			f := newSuggestFixture(t, nil)
			f.s.suggestIdleWaitFor = 2 * time.Second
			f.setList(mirror.StatusWaiting, c.since)
			tr := &suggestTracker{}
			for _, m := range suggestBackfill() {
				m.StatusSince = tokOwn
				tr.observe(m)
			}
			if c.want {
				go func() {
					time.Sleep(150 * time.Millisecond)
					f.setList(mirror.StatusIdle, tokOwn)
				}()
			}
			start := time.Now()
			if got := f.s.suggestTurn(operatorCtx(), "ws", "main", "@1", tr); got != c.want || f.gen.n() != map[bool]int{true: 1}[c.want] {
				t.Fatalf("offered %v, calls %d", got, f.gen.n())
			}
			if d := time.Since(start); !c.want && d > 500*time.Millisecond {
				t.Fatalf("its own prompt was waited out (%v), not taken at once", d)
			}
		})
	}
}

// TestATokenThatMovesToANonBusyStatusEndsATurn: a turn the stream never saw
// busy (it started and ended between two re-resolves) still ends — its idle's
// token moved; the FIRST token a stream names (its backfill) does not end
// one, nor does a busy one, and a Reset forgets the token.
func TestATokenThatMovesToANonBusyStatusEndsATurn(t *testing.T) {
	tr := &suggestTracker{}
	steps := []struct {
		m    mirror.Msg
		ends bool
	}{
		{mirror.Msg{Reset: true, StatusSince: tokBefore, Busy: true, Events: []cctranscript.Event{{Kind: cctranscript.KindPrompt, Text: "go"}}}, false},
		{mirror.Msg{Busy: true, StatusSince: tokBusy1}, false}, // a busy move
		{mirror.Msg{Busy: true, StatusSince: tokBusy2}, false}, // and another
		{mirror.Msg{StatusSince: tokBetween}, true},            // the marker fell (and moved)
		{mirror.Msg{StatusSince: tokOwn}, true},                // a short turn, never seen busy
		{mirror.Msg{StatusSince: tokOwn}, false},               // nothing moved
		{mirror.Msg{Reset: true, StatusSince: tokOwn + 1}, false},
		{mirror.Msg{StatusSince: tokOwn + 2}, true},
	}
	for i, st := range steps {
		if got := tr.observe(st.m); got != st.ends {
			t.Fatalf("step %d: ends=%v, want %v", i, got, st.ends)
		}
		if st.ends && tr.lastEnd().since != st.m.StatusSince {
			t.Fatalf("step %d: the turn end's token %d", i, tr.lastEnd().since)
		}
	}
}

// TestNothingIsSpentPastTheCutoff: a turn end older than ui.SuggestCutoff
// makes no call (the page would not show it); a fresh one makes one and says
// pending first, with how long ago the turn ended.
func TestNothingIsSpentPastTheCutoff(t *testing.T) {
	f := newSuggestFixture(t, nil)
	f.s.suggestIdleWaitFor = 0 // the real cutoff
	tr := &suggestTracker{}
	for _, m := range suggestBackfill() {
		tr.observe(m)
	}
	late := suggestEnd{since: suggestTok, at: time.Now().Add(-ui.SuggestCutoff - time.Second)}
	if f.s.suggestTurnAt(operatorCtx(), "ws", "main", "@1", tr, late, func(time.Duration) { t.Error("pending past the cutoff") }) || f.gen.n() != 0 {
		t.Fatalf("past the cutoff: calls %d", f.gen.n())
	}
	var pending time.Duration = -1
	fresh := suggestEnd{since: suggestTok, at: time.Now().Add(-time.Second)}
	if !f.s.suggestTurnAt(operatorCtx(), "ws", "main", "@1", tr, fresh, func(d time.Duration) { pending = d }) || f.gen.n() != 1 {
		t.Fatalf("fresh: calls %d", f.gen.n())
	}
	if pending < time.Second || pending >= ui.SuggestCutoff {
		t.Fatalf("pending said %v", pending)
	}
}

// TestNoSetForATurnTheStreamHasMovedPast (audit round 1, MED): the operator
// sent the next message within the list's refresh lag — the STREAM is busy
// again while the list still reads the ended turn's idle. Nothing is
// generated for the turn that is over, and the page is not told pending.
func TestNoSetForATurnTheStreamHasMovedPast(t *testing.T) {
	f := newSuggestFixture(t, nil)
	f.setList(mirror.StatusIdle, tokOwn)
	// The busy marker arrives before its prompt's record (or a queued message
	// started the turn): the tail still ends in the reply, so only the
	// stream's own state says the turn is over.
	// The busy marker with a newer token, and one that names none (only busy
	// says it).
	for _, next := range []int64{tokOwn + 10, 0} {
		tok := next
		tr := &suggestTracker{}
		for _, m := range suggestBackfill() {
			m.StatusSince = tokOwn
			tr.observe(m)
		}
		end := tr.lastEnd()
		busy := mirror.Msg{Busy: true, StatusSince: next}
		tr.observe(busy)
		if f.s.suggestTurnAt(operatorCtx(), "ws", "main", "@1", tr, end, func(time.Duration) { t.Errorf("token %d: pending for a turn that is over", tok) }) || f.gen.n() != 0 {
			t.Fatalf("token %d: calls %d", tok, f.gen.n())
		}
	}
	// Positive control: the same, the stream still at the turn's end.
	tr2 := &suggestTracker{}
	for _, m := range suggestBackfill() {
		m.StatusSince = tokOwn
		tr2.observe(m)
	}
	if !f.s.suggestTurnAt(operatorCtx(), "ws", "main", "@1", tr2, tr2.lastEnd(), nil) || f.gen.n() != 1 {
		t.Fatalf("control: calls %d", f.gen.n())
	}
}

// TestWithNoDatabaseTurningThemOnStillResumesAPause (audit round 1, LOW):
// no Prefs, so the toggle cannot be stored — but on=1 still clears an
// auto-pause, and Account offers the form while paused.
func TestWithNoDatabaseTurningThemOnStillResumesAPause(t *testing.T) {
	f := newSuggestFixture(t, nil)
	f.eng.PerOwner, f.eng.Global = 10_000, 10_000
	for i := 0; i < suggest.AutoPauseMinShown; i++ {
		sub := "sub-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		b, _ := suggest.Build(suggest.Input{Tail: []suggest.Entry{{Role: suggest.RoleAssistant, Text: suggestReply + " " + sub}}})
		set, _, _ := f.eng.Suggest(context.Background(), 1, sub, b, int64(i+1))
		f.eng.Record(operatorCtx(), 1, sub, set.Fingerprint, suggest.EventShown, 0)
	}
	r, _ := http.NewRequestWithContext(operatorCtx(), http.MethodGet, "/account", nil)
	if html := f.s.accountSuggestSection(r); !strings.Contains(html, `data-state="paused"`) || !strings.Contains(html, `name="on" value="1"`) {
		t.Fatalf("paused, no database: %s", html)
	}
	req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/account/suggest", strings.NewReader("on=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", sessionCookieFor(f.s))
	req.Header.Set("Origin", f.ts.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || suggestStats(t, f.eng).Paused {
		t.Fatalf("on=1 with no database: %d, paused %v", resp.StatusCode, suggestStats(t, f.eng).Paused)
	}
}

// TestNoTokenNoChips: a host that names no status token on the stream (an
// older muster-mirror) gets no set at all — its turn end cannot be told from
// the idle before the turn — and no call is made; the same view with the
// token (positive control) gets one.
func TestNoTokenNoChips(t *testing.T) {
	f := newSuggestFixture(t, nil)
	tr := &suggestTracker{}
	ended := false
	for _, m := range suggestBackfill() {
		m.StatusSince = 0
		ended = tr.observe(m) || ended
	}
	if !ended {
		t.Fatal("the backfill of an idle session must still end a turn")
	}
	f.s.suggestIdleWaitFor = 2 * time.Second
	start := time.Now()
	if f.s.suggestTurn(operatorCtx(), "ws", "main", "@1", tr) || f.gen.n() != 0 {
		t.Fatalf("no token: offered, calls %d", f.gen.n())
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("no token: waited %v for a status it can never name", d)
	}
	if !f.turnEnd(t) || f.gen.n() != 1 {
		t.Fatalf("control: calls %d", f.gen.n())
	}
}

// TestAResumedViewOffersNothingForTheTurnItResumedInto: a reloaded page
// resumes its socket (no backfill, no prompt index); its resume marker on an
// idle session ends no turn, so the row stays away for that turn — even when
// a set for it exists — until a whole new turn.
func TestAResumedViewOffersNothingForTheTurnItResumedInto(t *testing.T) {
	tr := &suggestTracker{resumed: true}
	if tr.observe(mirror.Msg{Offset: 4096, StatusSince: tokOwn}) {
		t.Fatal("the resume marker ended a turn")
	}
	if tr.observe(mirror.Msg{Events: []cctranscript.Event{{Kind: cctranscript.KindText, Text: p2Reply}}}) {
		t.Fatal("a reply of the resumed turn ended it")
	}
}

// suggestStats is eng's counts for the operator (user 1).
func suggestStats(t *testing.T, eng *suggest.Engine) suggest.Tally {
	t.Helper()
	st, err := eng.Stats(operatorCtx(), 1)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// keylessGen is a model the owner has no key for.
type keylessGen struct{ recordingGen }

func (*keylessGen) CanGenerate(context.Context, int64) error { return suggest.ErrNoKey }

// TestAnOwnerWithNoKeyIsNeverToldPending: no OpenRouter key, no set can come
// — the page is not told to reserve a row for one (no flash of an empty row
// at every turn end), and no call is made.
func TestAnOwnerWithNoKeyIsNeverToldPending(t *testing.T) {
	g := &keylessGen{}
	f := newSuggestFixture(t, func(_ *fakeMirror, ext *Extensions) { ext.Suggest.Gen = g })
	tr := &suggestTracker{}
	for _, m := range suggestBackfill() {
		tr.observe(m)
	}
	if f.s.suggestTurnAt(operatorCtx(), "ws", "main", "@1", tr, suggestEnd{since: suggestTok, at: time.Now()}, func(time.Duration) { t.Error("pending with no key") }) || g.n() != 0 {
		t.Fatalf("calls %d", g.n())
	}
}

// postEvent posts the chip row's report.
func (f *suggestFixture) postEvent(t *testing.T, form url.Values) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.ts.URL+ui.SuggestEventPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", sessionCookieFor(f.s))
	req.Header.Set("Origin", f.ts.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestTheChipRowsReportsCountAndCarryNoText: shown, filled and sent count
// once each against the set served; a send adds its edit distance; any field
// beyond the fixed ones (text) is refused; another window's address 404s.
func TestTheChipRowsReportsCountAndCarryNoText(t *testing.T) {
	f := newSuggestFixture(t, nil)
	f.turnEnd(t)
	fp := f.get(t).Fingerprint
	base := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "fingerprint": {fp}}
	with := func(kv ...string) url.Values {
		v := url.Values{}
		for k, x := range base {
			v[k] = x
		}
		for i := 0; i+1 < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v
	}
	for _, c := range []struct {
		form url.Values
		code int
	}{
		{with("event", "shown"), http.StatusNoContent},
		{with("event", "shown"), http.StatusNoContent}, // counted once
		{with("event", "filled"), http.StatusNoContent},
		{with("event", "filled", "idx", "0"), http.StatusBadRequest}, // no chip index is reported any more
		{with("event", "sent", "dist", "4"), http.StatusNoContent},
		{with("event", "sent", "dist", "4", "text", "Go with (a)"), http.StatusBadRequest},
		{with("event", "typed"), http.StatusBadRequest},
		{with("event", "sent", "dist", "-1"), http.StatusBadRequest},
		{with("event", "shown", "fingerprint", "not hex"), http.StatusBadRequest},
		{with("event", "shown", "session", "nope"), http.StatusNotFound},
	} {
		if code := f.postEvent(t, c.form); code != c.code {
			t.Fatalf("%v: %d, want %d", c.form, code, c.code)
		}
	}
	if st := suggestStats(t, f.eng); st != (suggest.Tally{Shown: 1, Filled: 1, Sent: 1, EditSum: 4}) {
		t.Fatalf("tally %+v", st)
	}
}

// TestAPausedUserIsToldSoAndTheToggleResumesThem: once the auto-pause paused
// a user, nothing is generated or served (with the reason); turning
// suggestions back on in Account resumes them, and the Account page's own
// form comes back to the page.
func TestAPausedUserIsToldSoAndTheToggleResumesThem(t *testing.T) {
	prefs := &suggestPrefs{}
	f := newSuggestFixture(t, func(_ *fakeMirror, ext *Extensions) { ext.Suggest.Prefs = prefs })
	f.eng.PerOwner, f.eng.Global = 10_000, 10_000
	// 200 sets shown, none tapped.
	for i := 0; i < suggest.AutoPauseMinShown; i++ {
		sub := "sub-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		b, _ := suggest.Build(suggest.Input{Tail: []suggest.Entry{{Role: suggest.RoleAssistant, Text: suggestReply + " " + sub}}})
		set, _, err := f.eng.Suggest(context.Background(), 1, sub, b, int64(i+1))
		if err != nil || !f.eng.Record(operatorCtx(), 1, sub, set.Fingerprint, suggest.EventShown, 0) {
			t.Fatalf("set %d: %v", i, err)
		}
	}
	if v := f.get(t); len(v.Suggestions) != 0 || v.Reason != suggestBlockedPaused {
		t.Fatalf("paused: %+v", v)
	}
	// The composer says so in one line (and draws no chip row); Account shows
	// the readout of the counts that paused it.
	_, page := mirrorGet(t, f.s, f.ts, ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}.ViewPath())
	if !strings.Contains(page, `id="mv-suggest-paused"`) || !strings.Contains(page, ui.SuggestPausedText) || strings.Contains(page, `id="mv-suggest"`) {
		t.Fatal("the paused composer line (and no row)")
	}
	r, _ := http.NewRequestWithContext(operatorCtx(), http.MethodGet, "/account", nil)
	html := f.s.accountSuggestSection(r)
	for _, want := range []string{`data-state="paused"`, `id="suggest-usage-shown" class="tabular-nums text-fg">200<`,
		`id="suggest-usage-filled" class="tabular-nums text-fg">0<`, `id="suggest-usage-rate" class="tabular-nums text-fg">0.0%<`,
		`id="suggest-usage-paused" class="tabular-nums text-fg">yes<`, "fewer than 5% of them were tapped"} {
		if !strings.Contains(html, want) {
			t.Fatalf("Account lacks %s:\n%s", want, html)
		}
	}
	if f.turnEnd(t) {
		t.Fatal("a paused user's turn end generated")
	}
	form := url.Values{"on": {"1"}, "back": {"account"}}
	req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/account/suggest", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", sessionCookieFor(f.s))
	req.Header.Set("Origin", f.ts.URL)
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account?suggest=saved#suggest" {
		t.Fatalf("back to Account: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if suggestStats(t, f.eng).Paused || f.eng.Enabled(context.Background(), 1) != "" {
		t.Fatal("turning suggestions back on did not resume")
	}
}

// TestTheAccountSectionSaysTheStateAndOffersTheToggle: on (a form that turns
// it off), off (one that turns it on), paused (one that turns it back on),
// and off for everyone (no form).
func TestTheAccountSectionSaysTheStateAndOffersTheToggle(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(*suggest.Engine, *suggestPrefs)
		state string
		on    string // the form's on value, "" for no form
	}{
		{"on", func(*suggest.Engine, *suggestPrefs) {}, "on", "0"},
		{"off", func(_ *suggest.Engine, p *suggestPrefs) { p.off = true }, "off", "1"},
		{"server off", func(e *suggest.Engine, _ *suggestPrefs) { e.Off = true }, "server-off", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			prefs := &suggestPrefs{}
			f := newSuggestFixture(t, func(_ *fakeMirror, ext *Extensions) { ext.Suggest.Prefs = prefs })
			c.setup(f.eng, prefs)
			r, _ := http.NewRequestWithContext(operatorCtx(), http.MethodGet, "/account", nil)
			html := f.s.accountSuggestSection(r)
			if !strings.Contains(html, `id="suggest-state" data-state="`+c.state+`"`) {
				t.Fatalf("state: %s", html)
			}
			hasForm := strings.Contains(html, `action="/account/suggest"`)
			if hasForm != (c.on != "") || (c.on != "" && !strings.Contains(html, `name="on" value="`+c.on+`"`)) {
				t.Fatalf("form: %s", html)
			}
		})
	}
}

// TestOnlyAClaudeWindowOnAnInteractiveHostGetsTheRow: suggestPath, what draws
// the chip row and opens the socket's tracker, is "" for a read-only host, a
// Hermes window, the kill switch and the user's toggle — and the window's URL
// otherwise.
func TestOnlyAClaudeWindowOnAnInteractiveHostGetsTheRow(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fakeMirror, *Extensions)
		want   bool
	}{
		{"claude window", nil, true},
		{"read-only host", func(fm *fakeMirror, _ *Extensions) { fm.interactive = false }, false},
		{"hermes", func(fm *fakeMirror, _ *Extensions) {
			fm.windows = []mirror.Window{{Session: "main", ID: "@1", Index: 1, Active: true, Runtime: mirror.RuntimeHermes, AgentSession: "local-" + strings.Repeat("a", 64)}}
		}, false},
		{"kill switch", func(_ *fakeMirror, ext *Extensions) { ext.Suggest.Off = true }, false},
		{"user off", func(_ *fakeMirror, ext *Extensions) { ext.Suggest.Prefs = &suggestPrefs{off: true} }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newSuggestFixture(t, c.mutate)
			// What the socket's tracker is opened on, directly: the page's
			// composer also drops the row for a read-only host or Hermes, so the
			// page alone cannot see this gate.
			h, e, ok := f.s.suggestWindow(operatorCtx(), "ws", "main", "@1")
			if !ok {
				t.Fatal("the window is not listed")
			}
			if got := f.s.suggestPath(operatorCtx(), h, e) != ""; got != c.want {
				t.Fatalf("suggestPath gives a URL: %v, want %v", got, c.want)
			}
			_, body := mirrorGet(t, f.s, f.ts, ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}.ViewPath())
			if strings.Contains(body, `id="mv-suggest-paused"`) {
				t.Fatal("a paused line for a user who is not paused")
			}
			if got := strings.Contains(body, `id="mv-suggest"`); got != c.want {
				t.Fatalf("row drawn %v, want %v", got, c.want)
			}
			if c.want && !strings.Contains(body, `data-src="/mirror/view/suggest?host=ws&amp;session=main&amp;window=%401"`) {
				t.Fatalf("the row's source: %s", body)
			}
		})
	}
}
