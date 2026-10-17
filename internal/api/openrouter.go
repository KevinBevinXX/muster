package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/searchrank"
)

// OpenRouter model-search proxy. The dispatch modal's model field queries this
// instead of hitting OpenRouter from the browser (avoids CORS + keeps it server-
// cached). OpenRouter's /api/v1/models is public (no key) and returns the full
// catalogue; we cache it and filter by the query. Results are returned as the
// agent-runtime slug form (`openrouter/<id>`) so the value is submit-ready.

// openRouterModelsURL is OpenRouter's public model list. A var so a browser-test
// binary (cmd/muster-server, `-tags e2eterm`) can point it at a fake with
// UseOpenRouterModelsURL; nothing else does.
var openRouterModelsURL = "https://openrouter.ai/api/v1/models"

// UseOpenRouterModelsURL points the model search at another catalogue. Only the
// e2eterm build calls it (its fake upstream); a deployable binary never does.
func UseOpenRouterModelsURL(u string) { openRouterModelsURL = u }

const (
	openRouterCacheTTL  = time.Hour
	openRouterMaxResult = 25
)

type modelCache struct {
	mu      sync.Mutex
	ids     []string // agent-runtime slugs: "openrouter/<id>"
	fetched time.Time
}

var orCache = &modelCache{}

// models returns the cached OpenRouter model slugs, refreshing past the TTL. On
// a fetch error it serves the stale cache (better than nothing for a picker).
func (c *modelCache) models(ctx context.Context) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.ids) > 0 && time.Since(c.fetched) < openRouterCacheTTL {
		return c.ids
	}
	ids, err := fetchOpenRouterModels(ctx)
	if err != nil || len(ids) == 0 {
		return c.ids // stale (possibly empty) on error
	}
	c.ids = ids
	c.fetched = time.Now()
	return c.ids
}

func fetchOpenRouterModels(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openRouterModelsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, err
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			ids = append(ids, "openrouter/"+m.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// handleOpenRouterModels serves GET /api/openrouter/models?q=…: a JSON array of
// the agent-runtime model slugs that match q, BEST FIRST, capped.
//
// 🔴 RANKED BEFORE THE CAP, BY THE PICKERS' OWN RULE (internal/searchrank), on
// the name the picker shows (the slug without "openrouter/"). Capping first, in
// alphabetical order, answered a common query with a page of substrings and cut
// the model whose name STARTS with it — and the picker can only rank what it was
// sent. Ties keep the alphabetical order.
func (s *Server) handleOpenRouterModels(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	all := orCache.models(r.Context())
	type hit struct {
		id   string
		tier int
	}
	hits := make([]hit, 0, len(all))
	for _, id := range all {
		if t := searchrank.Tier(strings.TrimPrefix(id, "openrouter/"), q); t >= 0 {
			hits = append(hits, hit{id, t})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].tier < hits[j].tier })
	out := make([]string, 0, openRouterMaxResult)
	for _, h := range hits {
		if len(out) >= openRouterMaxResult {
			break
		}
		out = append(out, h.id)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=300")
	_ = json.NewEncoder(w).Encode(out)
}
