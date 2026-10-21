package agentgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// checkoutReadTimeout bounds one /healthz read. ccd answers it from memory.
const checkoutReadTimeout = 5 * time.Second

// Checkout reads the repository checkout a claude-code agent's ccd reports on
// /healthz (`repo`; cmd/ccd/checkout.go): the input internal/agentkickoff needs
// to hold a first turn, unstamped, until the repository exists
// (agents.DecideFirstTurn). It contacts the runtime but sends it no turn.
//
// A pod with no repository reports no `repo`, which is [agents.CheckoutUnknown]
// with a nil error. A read that fails is an error; the caller decides what an
// unread checkout means (agents.DecideFirstTurn: as before, unless the row
// already recorded a wait).
//
// ⚠ IT ACCEPTS A 503. ccd's /healthz answers non-200 when the session is
// unhealthy (a CLI in crash_loop), and its body still carries `repo`; the probe
// is a liveness signal, not this read's.
func (g *Gateway) Checkout(ctx context.Context, t Target) (agents.CheckoutStatus, error) {
	if t.ep.IsZero() {
		return agents.CheckoutStatus{}, fmt.Errorf("read checkout: the target has no address")
	}
	ctx, cancel := context.WithTimeout(ctx, checkoutReadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(t.ep.URL(), "/")+"/healthz", nil)
	if err != nil {
		return agents.CheckoutStatus{}, err
	}
	// 🔴 A REDIRECT IS NOT FOLLOWED. /healthz is the agent's own; a 3xx is not
	// it, and following one would read another host's body as this agent's
	// checkout (and dial wherever it points). A copy: the shared client's
	// turns are untouched.
	client := *g.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return agents.CheckoutStatus{}, fmt.Errorf("read checkout: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return agents.CheckoutStatus{}, fmt.Errorf("read checkout: /healthz answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return agents.CheckoutStatus{}, fmt.Errorf("read checkout: %w", err)
	}
	return ParseHealthzCheckout(body)
}

// ParseHealthzCheckout extracts `repo` from a ccd /healthz body. Exported so
// cmd/ccd's test can feed it the body ccd's own handler writes: the two sides of
// this wire meet only in a running pod otherwise.
func ParseHealthzCheckout(body []byte) (agents.CheckoutStatus, error) {
	var h struct {
		Repo *agents.CheckoutStatus `json:"repo"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		return agents.CheckoutStatus{}, fmt.Errorf("read checkout: /healthz body: %w", err)
	}
	if h.Repo == nil {
		return agents.CheckoutStatus{}, nil
	}
	return *h.Repo, nil
}
