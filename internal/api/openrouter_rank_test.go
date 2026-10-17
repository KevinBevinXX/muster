package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestModelSearchRanksBeforeTheCap: the catalogue holds 30 models whose names
// merely CONTAIN "kit" (alphabetically first) and one whose name STARTS with it
// (alphabetically after all 30), so an answer capped at 25 in alphabetical order cuts
// exactly the best match. Ranked first, it leads; the word-start match is next.
func TestModelSearchRanksBeforeTheCap(t *testing.T) {
	ids := []string{}
	for i := 0; i < 30; i++ {
		ids = append(ids, fmt.Sprintf("aa/model-%02dkitx", i)) // substring (rank 2)
	}
	ids = append(ids, "zz/tool-kitten") // word start (rank 1)
	ids = append(ids, "zzz-kitchen")    // word start (rank 1), after the other one alphabetically
	ids = append(ids, "kite/runner")    // prefix (rank 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var body struct {
			Data []map[string]string `json:"data"`
		}
		for _, id := range ids {
			body.Data = append(body.Data, map[string]string{"id": id})
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	oldURL, oldCache := openRouterModelsURL, orCache
	UseOpenRouterModelsURL(srv.URL)
	orCache = &modelCache{}
	t.Cleanup(func() { openRouterModelsURL, orCache = oldURL, oldCache })

	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleOpenRouterModels(rec, httptest.NewRequest(http.MethodGet, "/api/openrouter/models?q=kit", nil))
	var got []string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != openRouterMaxResult {
		t.Fatalf("%d results, want the cap (%d)", len(got), openRouterMaxResult)
	}
	want := []string{"openrouter/kite/runner", "openrouter/zz/tool-kitten", "openrouter/zzz-kitchen", "openrouter/aa/model-00kitx"}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("result %d = %q, want %q (best match first, before the cap): %v", i, got[i], w, got[:5])
		}
	}
}
