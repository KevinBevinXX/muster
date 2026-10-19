package main

import (
	"net/http"
	"strings"
	"testing"
)

// TestPluginsRetryPostsTheIDAndReportsARefusal: the verb posts to the message's
// retry route with the hook credential and no body; a 409 from the server (not
// retryable) is exit 5 with the server's reason; a non-numeric id is refused
// locally with no request.
func TestPluginsRetryPostsTheIDAndReportsARefusal(t *testing.T) {
	h := newHarness(t)
	h.json("POST /api/plugins/messages/6/retry", http.StatusAccepted, `{"id":6,"status":"queued","retrying":true}`)
	got := h.runCLI("plugins", "retry", "6")
	if got.code != exitOK || !strings.Contains(got.stdout, `"retrying": true`) {
		t.Fatalf("exit %d stdout %q stderr %q", got.code, got.stdout, got.stderr)
	}
	reqs := h.nonHealthRequests()
	if len(reqs) != 1 || reqs[0].method+" "+reqs[0].uri != "POST /api/plugins/messages/6/retry" || reqs[0].auth != "Bearer "+testToken {
		t.Fatalf("requests = %+v", reqs)
	}

	h2 := newHarness(t)
	h2.json("POST /api/plugins/messages/7/retry", http.StatusConflict, `{"error":"That message cannot be retried: your chief already received it."}`)
	if got := h2.runCLI("plugins", "retry", "7"); got.code != exitConflict || !strings.Contains(got.stderr, "already received it") {
		t.Fatalf("a refused retry: exit %d stderr %q", got.code, got.stderr)
	}

	h3 := newHarness(t)
	if got := h3.runCLI("plugins", "retry", "six"); got.code != exitUsage || len(h3.nonHealthRequests()) != 0 {
		t.Fatalf("a non-numeric id: exit %d, %d request(s)", got.code, len(h3.nonHealthRequests()))
	}
}
