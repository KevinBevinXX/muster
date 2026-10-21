// Package podagent is the part of an agent pod's in-pod supervisor that does not
// depend on which agent runtime the pod runs: muster's agent-facing routes as the
// pod reaches them, git's routing through muster's connector proxy, and the
// agent's repository checkout.
//
// Two supervisors use it: cmd/ccd (the claude-code kind) and cmd/hermesd (the
// hermes kind). The code was ccd's; it moved here unchanged in behaviour when
// hermesd needed the same rules, so that each rule has one implementation (the
// messages a person reads name the supervisor that wrote them, through
// [Voice]).
package podagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// EnvConnectURL is the base URL of muster's agent-facing connector surface,
	// set on the pod only when the deployment offers it.
	EnvConnectURL = "MUSTER_CONNECT_URL"
	// EnvAgentToken is the agent's OWN per-agent muster token.
	EnvAgentToken = "MUSTER_HOOK_TOKEN"
)

// NoRedirects is the client every call to muster uses. 🔴 IT NEVER FOLLOWS A
// REDIRECT: Go's client resends custom headers (X-Muster-Token among them) to
// a redirect's target, even on another host. muster's agent routes do not
// redirect, so a redirect here is something to refuse, not to follow.
var NoRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// MusterCall sends one request to muster's agent routes with the agent's own
// token, decoding a 2xx JSON answer into out (when non-nil), and reports the
// status. A non-2xx is NOT an error here: callers that branch on the status
// (the approval relay) read it; MusterGET and MusterPOST turn it into one.
// client nil is NoRedirects.
func MusterCall(ctx context.Context, client *http.Client, method, base, token, path string, in, out any) (int, []byte, error) {
	if client == nil {
		client = NoRedirects
	}
	var rd io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-Muster-Token", token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("reaching muster: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, raw, fmt.Errorf("muster's answer is not JSON: %w", err)
		}
	}
	return resp.StatusCode, raw, nil
}

// MusterGET reads one of muster's agent JSON routes into out. It is a GET and
// files nothing.
func MusterGET(ctx context.Context, base, token, path string, out any) error {
	return musterExpect2xx(MusterCall(ctx, nil, http.MethodGet, base, token, path, nil, out))
}

// MusterPOST sends a JSON body to one of muster's agent routes.
func MusterPOST(ctx context.Context, base, token, path string, body any) error {
	return musterExpect2xx(MusterCall(ctx, nil, http.MethodPost, base, token, path, body, nil))
}

func musterExpect2xx(status int, raw []byte, err error) error {
	if err != nil && status == 0 {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("muster answered %d: %s", status, strings.TrimSpace(string(raw)))
	}
	return err
}

// LastLines is the last n non-empty lines of s, joined with "; ", control
// characters removed and capped at max bytes.
func LastLines(s string, n, max int) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, l)
		if l = strings.TrimSpace(l); l != "" {
			keep = append(keep, l)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	out := strings.Join(keep, "; ")
	if len(out) > max {
		out = out[:max] + "…"
	}
	return out
}
