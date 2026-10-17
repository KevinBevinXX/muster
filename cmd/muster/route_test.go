package main

import (
	"net/http"
	"strings"
	"testing"
)

const testRouteID = "rt_0123456789abcdef01234567"

// TestRouteReplyPostsTheAnswer: --body and --body-file - each POST the answer as
// {"body"} to the route's reply door, with the configured credential.
func TestRouteReplyPostsTheAnswer(t *testing.T) {
	for _, c := range []struct {
		name  string
		args  []string
		stdin string
	}{
		{"--body", []string{"route", "reply", testRouteID, "--body", "Noon works."}, ""},
		{"--body-file -", []string{"route", "reply", testRouteID, "--body-file", "-"}, "Noon works."},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.json("POST /api/routes/"+testRouteID+"/reply", http.StatusOK, `{"route_id":"`+testRouteID+`","state":"relayed"}`)
			got := h.runCLIStdin(strings.NewReader(c.stdin), c.args...)
			if got.code != exitOK || !strings.Contains(got.stdout, `"state": "relayed"`) {
				t.Fatalf("exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
			}
			reqs := h.nonHealthRequests()
			if len(reqs) != 1 || reqs[0].method != http.MethodPost || reqs[0].uri != "/api/routes/"+testRouteID+"/reply" ||
				reqs[0].body != `{"body":"Noon works."}` || reqs[0].auth == "" {
				t.Fatalf("requests = %+v", reqs)
			}
		})
	}
}

// TestRouteReplyRefusesBadInputWithoutCallingTheServer.
func TestRouteReplyRefusesBadInputWithoutCallingTheServer(t *testing.T) {
	for _, args := range [][]string{
		{"route", "reply", "rt_short", "--body", "x"},
		{"route", "reply", "../../api/tasks", "--body", "x"},
		{"route", "reply", testRouteID},
		{"route", "reply", testRouteID, "--body", "   "},
		{"route", "reply", testRouteID, "--body", "x", "--body-file", "-"},
	} {
		h := newHarness(t)
		got := h.runCLIStdin(strings.NewReader("y"), args...)
		if got.code != exitUsage {
			t.Errorf("%v: exit %d, want %d (usage); stderr %q", args, got.code, exitUsage, got.stderr)
		}
		if n := len(h.nonHealthRequests()); n != 0 {
			t.Errorf("%v: %d request(s) made", args, n)
		}
	}
}
