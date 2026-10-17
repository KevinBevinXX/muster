package api

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/ui"
)

// stalledUpload starts an upload whose body sends its multipart header and a
// few bytes of the file, then stops. It returns a channel with the response
// (or the error) and a func that unblocks the body.
func (w *upWorld) stalledUpload(t *testing.T) (<-chan *http.Response, func()) {
	t.Helper()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		fw, _ := mw.CreateFormFile("file", "slow.png")
		_, _ = fw.Write(pngBytes[:20]) // and then nothing
	}()
	req, _ := http.NewRequest(http.MethodPost, w.ts.URL+ui.UploadPath, pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Cookie", sessionCookieFor(w.s))
	req.Header.Set("Origin", w.ts.URL)
	out := make(chan *http.Response, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			out <- nil
			return
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		out <- resp
	}()
	release := func() { pw.CloseWithError(errors.New("released")) }
	t.Cleanup(release)
	return out, release
}

// TestATricklingUploadIsCutOffAtItsReadDeadline: a body that stops arriving
// is answered 408 once the read deadline passes — the slot is not held for
// ever — and the next upload goes through.
func TestATricklingUploadIsCutOffAtItsReadDeadline(t *testing.T) {
	w := uploadServer(t, true, 1<<20)
	w.s.uploadLimits.readTimeout = 300 * time.Millisecond
	start := time.Now()
	out, _ := w.stalledUpload(t)
	select {
	case resp := <-out:
		if resp == nil || resp.StatusCode != http.StatusRequestTimeout {
			t.Fatalf("a stalled body was answered %+v; want 408", resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled body was still being waited on after 5s; the read deadline is 300ms")
	}
	if took := time.Since(start); took < 250*time.Millisecond {
		t.Errorf("answered after %v, before the deadline: refused for some other reason", took)
	}
	if resp, body := w.postFile(t, "next.png", pngBytes, upOpt{}); resp.StatusCode != http.StatusCreated {
		t.Errorf("the next upload: %d %q; want 201 (the stalled one's slot freed)", resp.StatusCode, body)
	}
}

// TestOneUsersUploadsAreCappedNotEveryones: a user's third concurrent upload
// is 429 while its first two are in flight; and (the limiter itself) another
// user is unaffected, under a process-wide ceiling.
func TestOneUsersUploadsAreCappedNotEveryones(t *testing.T) {
	w := uploadServer(t, true, 1<<20)
	w.s.uploadLimits.readTimeout = time.Minute
	_, rel1 := w.stalledUpload(t)
	_, rel2 := w.stalledUpload(t)
	// Wait until both hold their slots.
	deadline := time.Now().Add(5 * time.Second)
	for {
		w.s.uploadLimits.mu.Lock()
		n := w.s.uploadLimits.total
		w.s.uploadLimits.mu.Unlock()
		if n == uploadInFlightPerUser {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d uploads in flight; want %d", n, uploadInFlightPerUser)
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, body := w.postFile(t, "third.png", pngBytes, upOpt{})
	if resp.StatusCode != http.StatusTooManyRequests || body != UploadBusy {
		t.Errorf("a third concurrent upload: %d %q; want 429 %q", resp.StatusCode, body, UploadBusy)
	}
	rel1()
	rel2()

	var l uploadLimits
	var leaves []func()
	for i := 0; i < uploadInFlightPerUser; i++ {
		leave, err := l.enter(1)
		if err != nil {
			t.Fatal(err)
		}
		leaves = append(leaves, leave)
	}
	if _, err := l.enter(1); !errors.Is(err, errUserBusy) {
		t.Errorf("user 1 over its cap: %v; want errUserBusy", err)
	}
	if leave, err := l.enter(2); err != nil {
		t.Errorf("user 2 while user 1 is at its cap: %v; want a slot", err)
	} else {
		leave()
	}
	for uid := int64(10); len(leaves) < uploadInFlightTotal; uid++ {
		leave, err := l.enter(uid)
		if err != nil {
			t.Fatalf("user %d below the ceiling: %v", uid, err)
		}
		leaves = append(leaves, leave)
	}
	if _, err := l.enter(99); !errors.Is(err, errServerBusy) {
		t.Errorf("past the process-wide ceiling: %v; want errServerBusy", err)
	}
	leaves[0]()
	if leave, err := l.enter(1); err != nil {
		t.Errorf("after one of user 1's left: %v; want a slot", err)
	} else {
		leave()
	}
}

// TestAHEADOnALinkFetchesNoBody: HEAD answers the GET's headers from the
// object's metadata; the store is never asked for the bytes.
func TestAHEADOnALinkFetchesNoBody(t *testing.T) {
	w := uploadServer(t, true, 1<<20)
	rep := mustUpload(t, w, "shot.png", pngBytes)
	link := linkPath(t, strings.Fields(rep.Line[strings.Index(rep.Line, ") ")+2:])[0])
	gets := w.objs.Gets
	resp, body := w.do(t, http.MethodHead, link, false)
	if resp.StatusCode != http.StatusOK || len(body) != 0 {
		t.Fatalf("HEAD: %d with %d body bytes", resp.StatusCode, len(body))
	}
	if resp.ContentLength != int64(len(pngBytes)) {
		t.Errorf("HEAD Content-Length = %d; want %d", resp.ContentLength, len(pngBytes))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("HEAD Content-Type = %q", ct)
	}
	if w.objs.Gets != gets {
		t.Errorf("a HEAD fetched the object's body (%d GET(s))", w.objs.Gets-gets)
	}
	if resp, _ := w.do(t, http.MethodHead, strings.Replace(link, "sig=", "sig=x", 1), false); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a tampered HEAD: %d; want 404", resp.StatusCode)
	}
}

// TestARefusedLinkCannotForgeALogLineOrFloodTheLog: the public route's id is
// attacker-chosen and decoded — a %0A in it must not start a line of its own —
// and a burst of refusals is logged once, not once each.
func TestARefusedLinkCannotForgeALogLineOrFloodTheLog(t *testing.T) {
	w := uploadServer(t, true, 1<<20)
	w.logs.mu.Lock()
	w.logs.b.Reset()
	w.logs.mu.Unlock()
	forged := "/f/AAAA%0Auploads:%20user=1%20revoked%20EVERYTHING?exp=1&sig=x"
	if resp, _ := w.do(t, http.MethodGet, forged, false); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("forged id: %d; want 404", resp.StatusCode)
	}
	for i := 0; i < 50; i++ {
		w.do(t, http.MethodGet, "/f/BBBBBBBBBBBBBBBBBBBBBB?exp=1&sig=x", false)
	}
	logs := w.logs.String()
	if strings.Contains("\n"+logs, "\nuploads: user=1 revoked EVERYTHING") {
		t.Errorf("the forged id reached the log:\n%s", logs)
	}
	if n := strings.Count(logs, "link refused"); n < 1 || n > 2 {
		t.Errorf("%d refusal lines for 51 refusals inside a second; want 1 (or 2 across a second boundary):\n%s", n, logs)
	}
	if !strings.Contains(logs, "(malformed id)") {
		t.Errorf("CONTROL: the first refusal was not logged at all:\n%s", logs)
	}
}
