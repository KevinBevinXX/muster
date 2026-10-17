package askq

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The hook's half of the audit's scenarios (PR #130): it never lets go of
// answers muster took, and it can always withdraw what it registered.

// TestAnAnswerGivenAsTheHookGivesUpIsStillDelivered (audit scenario A, a
// network partition): the hook's polls fail until it gives up, while muster
// took the person's answer; the give-up's withdraw returns Answered, and the
// hook prints those answers instead of leaving the question to the terminal.
func TestAnAnswerGivenAsTheHookGivesUpIsStillDelivered(t *testing.T) {
	store := New()
	var key Key
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mirror/ask", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID        string          `json:"id"`
			ToolInput json.RawMessage `json:"toolInput"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		qs, _ := ParseInput(req.ToolInput)
		key = Key{Owner: 1, Source: "mirror", Scope: "laptop", Session: "s", ToolUseID: "t"}
		e, _ := store.Open(key, qs, time.Hour, req.ID)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": e.ID, "state": e.State})
	})
	// The partition: every poll fails — and meanwhile the person answers.
	mux.HandleFunc("GET /mirror/ask/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = store.Answer(key, map[string]string{"Which color do you prefer?": "Green", "Which pets do you have?": "Fish"})
		http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
	})
	mux.HandleFunc("DELETE /mirror/ask/{id}", func(w http.ResponseWriter, r *http.Request) {
		e, err := store.Withdraw(r.PathValue("id"), 1, "mirror", "laptop")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": e.ID, "state": e.State, "answers": e.Answers})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	out := testHook(srv.URL).Run(context.Background(), gatePayload(t))
	var got struct {
		HSO struct {
			UpdatedInput struct {
				Answers map[string]string `json:"answers"`
			} `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &got); err != nil || got.HSO.UpdatedInput.Answers["Which color do you prefer?"] != "Green" {
		t.Fatalf("the answers muster took were dropped: %q", out)
	}
}

// TestARegistrationWhoseAnswerWasLostIsWithdrawn (audit scenario C): muster
// creates the question but its answer never reaches the hook (a timeout). The
// hook named the question itself, so it withdraws it — muster does not go on
// offering a question nobody is holding.
func TestARegistrationWhoseAnswerWasLostIsWithdrawn(t *testing.T) {
	store := New()
	var created atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mirror/ask", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID        string          `json:"id"`
			ToolInput json.RawMessage `json:"toolInput"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		qs, _ := ParseInput(req.ToolInput)
		e, _ := store.Open(Key{Owner: 1, Source: "mirror", Scope: "laptop", Session: "s", ToolUseID: "t"}, qs, time.Hour, req.ID)
		created.Store(e.ID)
		time.Sleep(500 * time.Millisecond) // past the hook's client timeout
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("DELETE /mirror/ask/{id}", func(w http.ResponseWriter, r *http.Request) {
		e, err := store.Withdraw(r.PathValue("id"), 1, "mirror", "laptop")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": e.ID, "state": e.State})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	h := testHook(srv.URL)
	h.Client = &http.Client{Timeout: 150 * time.Millisecond}
	if out := h.Run(context.Background(), gatePayload(t)); out != nil {
		t.Fatalf("printed %s", out)
	}
	id, _ := created.Load().(string)
	e, ok := store.Get(id)
	if !ok || e.State != Withdrawn || e.Reason != ReasonGaveUp {
		t.Fatalf("the question muster created is %+v (found %v), want withdrawn by the hook", e, ok)
	}
}

// TestA504IsTransient (audit 🟢4): a gateway timeout in front of muster is
// retried, not taken for a refusal — the answer that follows is delivered.
func TestA504IsTransient(t *testing.T) {
	for _, status := range []int{http.StatusGatewayTimeout, http.StatusInternalServerError, http.StatusBadGateway} {
		store := New()
		var key Key
		var polls atomic.Int32
		mux := http.NewServeMux()
		mux.HandleFunc("POST /mirror/ask", func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				ID        string          `json:"id"`
				ToolInput json.RawMessage `json:"toolInput"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			qs, _ := ParseInput(req.ToolInput)
			key = Key{Owner: 1, Source: "mirror", Scope: "laptop", Session: "s", ToolUseID: "t"}
			e, _ := store.Open(key, qs, time.Hour, req.ID)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": e.ID, "state": e.State})
		})
		mux.HandleFunc("GET /mirror/ask/{id}", func(w http.ResponseWriter, r *http.Request) {
			if polls.Add(1) == 1 {
				http.Error(w, "blip", status)
				return
			}
			_, _ = store.Answer(key, map[string]string{"Which color do you prefer?": "Red", "Which pets do you have?": "Dog"})
			e, _ := store.Get(r.PathValue("id"))
			_ = json.NewEncoder(w).Encode(map[string]any{"id": e.ID, "state": e.State, "answers": e.Answers})
		})
		mux.HandleFunc("DELETE /mirror/ask/{id}", func(w http.ResponseWriter, r *http.Request) {
			e, _ := store.Withdraw(r.PathValue("id"), 1, "mirror", "laptop")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": e.ID, "state": e.State})
		})
		srv := httptest.NewServer(mux)
		h := testHook(srv.URL)
		h.GiveUpAfter = 5 * time.Second
		out := h.Run(context.Background(), gatePayload(t))
		srv.Close()
		if out == nil {
			t.Fatalf("a %d on one poll ended the hold", status)
		}
	}
}
