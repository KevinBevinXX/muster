package askq

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMuster plays muster's ask routes over a real Store, the way internal/api
// serves them, and records what the hook did.
type fakeMuster struct {
	store *Store
	mu    sync.Mutex
	opens []map[string]any
	gone  []string
	// refuse, when set, is the status every registration gets.
	refuse int
	// answerOnPoll answers the question at the first poll.
	answerOnPoll map[string]string
	// forget answers polls with 404 (a muster restart).
	forget bool
}

func (f *fakeMuster) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	auth := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer mmt_test" }
	mux.HandleFunc("POST /mirror/ask", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		var req struct {
			ID          string          `json:"id"`
			SessionID   string          `json:"sessionId"`
			ToolUseID   string          `json:"toolUseId"`
			ToolInput   json.RawMessage `json:"toolInput"`
			HoldSeconds int             `json:"holdSeconds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.opens = append(f.opens, map[string]any{"sessionId": req.SessionID, "toolUseId": req.ToolUseID, "holdSeconds": req.HoldSeconds})
		refuse := f.refuse
		f.mu.Unlock()
		if refuse != 0 {
			http.Error(w, "refused", refuse)
			return
		}
		qs, err := ParseInput(req.ToolInput)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		e, err := f.store.Open(Key{Owner: 1, Source: "mirror", Scope: "laptop", Session: req.SessionID, ToolUseID: req.ToolUseID}, qs, time.Duration(req.HoldSeconds)*time.Second, req.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": e.ID, "state": e.State})
	})
	mux.HandleFunc("GET /mirror/ask/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		forget, ans := f.forget, f.answerOnPoll
		f.mu.Unlock()
		if forget {
			http.NotFound(w, r)
			return
		}
		if ans != nil {
			if e, ok := f.store.Get(r.PathValue("id")); ok {
				_, _ = f.store.Answer(e.Key, ans)
			}
		}
		e, err := f.store.Poll(r.Context(), r.PathValue("id"), 1, "mirror", "laptop", 50*time.Millisecond)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": e.ID, "state": e.State, "answers": e.Answers})
	})
	mux.HandleFunc("DELETE /mirror/ask/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.gone = append(f.gone, r.PathValue("id"))
		f.mu.Unlock()
		e, err := f.store.Withdraw(r.PathValue("id"), 1, "mirror", "laptop")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": e.ID, "state": e.State, "answers": e.Answers})
	})
	return mux
}

func gatePayload(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/pretooluse-askuserquestion.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testHook(base string) Hook {
	return Hook{Base: base, Path: "/mirror/ask", Auth: func(r *http.Request) { r.Header.Set("Authorization", "Bearer mmt_test") },
		Hold: 2 * time.Second, Wait: 100 * time.Millisecond, GiveUpAfter: 300 * time.Millisecond, Client: &http.Client{Timeout: 5 * time.Second}}
}

// TestTheHookPrintsTheProvenPayloadForAPersonsAnswers: the real PreToolUse
// payload in, a person's answers from muster, and EXACTLY the proven output out.
func TestTheHookPrintsTheProvenPayloadForAPersonsAnswers(t *testing.T) {
	f := &fakeMuster{store: New(), answerOnPoll: map[string]string{"Which color do you prefer?": "Red", "Which pets do you have?": "Cat, Dog"}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	out := testHook(srv.URL).Run(context.Background(), gatePayload(t))
	want, _ := os.ReadFile("testdata/pretooluse-output-proven.json")
	sameJSON(t, out, []byte(strings.TrimSpace(string(want))))
	if len(f.opens) != 1 || f.opens[0]["sessionId"] != "8c1d1892-a9c0-4d90-bfdf-3c56848ac6fa" || f.opens[0]["toolUseId"] != "toolu_01RY27FfKTitvDGupDC74z2s" {
		t.Fatalf("registration %+v", f.opens)
	}
	if len(f.gone) != 0 {
		t.Fatalf("an answered question was withdrawn: %v", f.gone)
	}
}

// TestTheHookFailsOpenOnEveryFailure: nothing printed — the terminal's picker.
func TestTheHookFailsOpenOnEveryFailure(t *testing.T) {
	t.Run("muster down", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		base := srv.URL
		srv.Close()
		if out := testHook(base).Run(context.Background(), gatePayload(t)); out != nil {
			t.Fatalf("printed %s", out)
		}
	})
	for _, status := range []int{http.StatusNotFound, http.StatusConflict, http.StatusUnauthorized, http.StatusBadRequest, http.StatusServiceUnavailable} {
		t.Run("registration refused "+http.StatusText(status), func(t *testing.T) {
			f := &fakeMuster{store: New(), refuse: status}
			srv := httptest.NewServer(f.handler(t))
			defer srv.Close()
			if out := testHook(srv.URL).Run(context.Background(), gatePayload(t)); out != nil {
				t.Fatalf("printed %s", out)
			}
		})
	}
	t.Run("nobody answers before the hold ends", func(t *testing.T) {
		f := &fakeMuster{store: New()}
		srv := httptest.NewServer(f.handler(t))
		defer srv.Close()
		h := testHook(srv.URL)
		h.Hold = 400 * time.Millisecond
		done := make(chan []byte, 1)
		go func() { done <- h.Run(context.Background(), gatePayload(t)) }()
		select {
		case out := <-done:
			if out != nil {
				t.Fatalf("printed %s", out)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("held past a 400ms hold")
		}
		if len(f.gone) != 1 {
			t.Fatalf("the timed-out question was not withdrawn: %v", f.gone)
		}
	})
	t.Run("muster forgets the question", func(t *testing.T) {
		f := &fakeMuster{store: New(), forget: true}
		srv := httptest.NewServer(f.handler(t))
		defer srv.Close()
		if out := testHook(srv.URL).Run(context.Background(), gatePayload(t)); out != nil {
			t.Fatalf("printed %s", out)
		}
	})
	t.Run("muster goes away mid-hold", func(t *testing.T) {
		f := &fakeMuster{store: New()}
		srv := httptest.NewServer(f.handler(t))
		h := testHook(srv.URL)
		h.Hold = 10 * time.Second
		done := make(chan []byte, 1)
		go func() { done <- h.Run(context.Background(), gatePayload(t)) }()
		time.Sleep(150 * time.Millisecond)
		srv.CloseClientConnections()
		srv.Close()
		select {
		case out := <-done:
			if out != nil {
				t.Fatalf("printed %s", out)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the hook kept holding after muster went away (GiveUpAfter 300ms)")
		}
	})
	t.Run("answers that do not fit the questions", func(t *testing.T) {
		f := &fakeMuster{store: New()}
		mux := http.NewServeMux()
		mux.HandleFunc("POST /mirror/ask", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"x","state":"pending"}`))
		})
		mux.HandleFunc("GET /mirror/ask/x", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"id":"x","state":"answered","answers":{"Which color do you prefer?":"Red"}}`))
		})
		mux.HandleFunc("DELETE /mirror/ask/x", func(w http.ResponseWriter, r *http.Request) { f.gone = append(f.gone, "x") })
		srv := httptest.NewServer(mux)
		defer srv.Close()
		if out := testHook(srv.URL).Run(context.Background(), gatePayload(t)); out != nil {
			t.Fatalf("printed %s for a partial answer", out)
		}
	})
	t.Run("interrupted", func(t *testing.T) {
		f := &fakeMuster{store: New()}
		srv := httptest.NewServer(f.handler(t))
		defer srv.Close()
		ctx, cancel := context.WithCancel(context.Background())
		h := testHook(srv.URL)
		h.Hold = 10 * time.Second
		time.AfterFunc(200*time.Millisecond, cancel)
		if out := h.Run(ctx, gatePayload(t)); out != nil {
			t.Fatalf("printed %s", out)
		}
		if len(f.gone) != 1 {
			t.Fatalf("an interrupted hold was not withdrawn: %v", f.gone)
		}
	})
	t.Run("not AskUserQuestion", func(t *testing.T) {
		f := &fakeMuster{store: New()}
		srv := httptest.NewServer(f.handler(t))
		defer srv.Close()
		p := strings.Replace(string(gatePayload(t)), `"tool_name":"AskUserQuestion"`, `"tool_name":"Bash"`, 1)
		if out := testHook(srv.URL).Run(context.Background(), []byte(p)); out != nil || len(f.opens) != 0 {
			t.Fatalf("printed %s, registered %d", out, len(f.opens))
		}
	})
}
