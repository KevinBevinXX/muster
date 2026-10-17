package mirrorhost

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE HOST'S HALF OF THE CATALOG REQUEST, against a hand-driven "muster": a
// bare WebSocket that says welcome and then asks exactly what the test tells
// it to — including what a real muster would never send (a session the host
// does not list), so the host's OWN allowlist is what answers.

type listedBackend struct{ sessions []string }

func (b listedBackend) List(context.Context) ([]string, error) { return b.sessions, nil }
func (listedBackend) Attach(context.Context, string, bool, uint16, uint16) (Terminal, error) {
	return nil, io.EOF
}

// rawMuster serves one host connection: it records the hello, welcomes it,
// sends each of asks, and returns every message the host sent after the hello.
func rawMuster(t *testing.T, cfg Config, asks []mirror.Msg, wait time.Duration) (hello mirror.Msg, got []mirror.Msg) {
	t.Helper()
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ws.SetReadLimit(mirror.MaxFrame)
		ctx, cancel := context.WithTimeout(r.Context(), wait+5*time.Second)
		defer cancel()
		_, b, err := ws.Read(ctx)
		if err != nil {
			return
		}
		_ = json.Unmarshal(b, &hello)
		w1, _ := json.Marshal(mirror.Msg{Type: mirror.TypeWelcome, Version: hello.Version, Host: hello.Host})
		_ = ws.Write(ctx, websocket.MessageText, w1)
		for _, a := range asks {
			b, _ := json.Marshal(a)
			_ = ws.Write(ctx, websocket.MessageText, b)
		}
		rctx, rcancel := context.WithTimeout(ctx, wait)
		defer rcancel()
		for {
			typ, b, err := ws.Read(rctx)
			if err != nil {
				return
			}
			var m mirror.Msg
			if typ == websocket.MessageText && json.Unmarshal(b, &m) == nil && m.Type == mirror.TypeCataloged {
				got = append(got, m)
				if len(got) == len(asks) {
					return
				}
			}
		}
	}))
	defer srv.Close()
	cfg.URL, cfg.Token, cfg.Host, cfg.AllowHTTP = srv.URL, mirror.TokenPrefix+"x", "h", true
	cfg.Logger = log.New(io.Discard, "", 0)
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.RunOnce(ctx) }()
	select {
	case <-done:
	case <-time.After(wait + 10*time.Second):
		t.Fatal("the raw muster never finished")
	}
	return hello, got
}

// TestTheHostAnswersOnlyForWhatItListed: a request for a listed session is
// answered with the catalog; one for a session this host does not list — which
// muster's own check would never let through — is refused BY THE HOST, with no
// catalog in the answer.
func TestTheHostAnswersOnlyForWhatItListed(t *testing.T) {
	cfgDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cfgDir, "skills", "listed-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "skills", "listed-skill", "SKILL.md"), []byte("---\nname: listed-skill\ndescription: d\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Backend: listedBackend{[]string{"main"}}, Catalog: &CatalogScanner{Config: cfgDir, Home: t.TempDir(), SnippetsFile: "-"}}
	hello, got := rawMuster(t, cfg, []mirror.Msg{
		{Type: mirror.TypeCatalog, ID: 1, Session: "main"},
		{Type: mirror.TypeCatalog, ID: 2, Session: "secret"},
	}, 3*time.Second)
	if !hello.CatalogReport {
		t.Fatal("a current host's hello does not advertise its catalog")
	}
	if len(got) != 2 {
		t.Fatalf("%d answers, want 2: %+v", len(got), got)
	}
	byID := map[uint32]mirror.Msg{got[0].ID: got[0], got[1].ID: got[1]}
	if a := byID[1]; a.Error != "" || a.Catalog == nil || len(a.Catalog.Commands) != 1 || a.Catalog.Commands[0].Name != "listed-skill" {
		t.Fatalf("the listed session's answer: %+v", a)
	}
	if a := byID[2]; a.Error == "" || a.Catalog != nil {
		t.Fatalf("an unlisted session was answered: %+v", a)
	}
}

// TestAHostWithNoCatalogAdvertisesNoneAndAnswersNothing: what an older
// muster-mirror does (NoCatalog plays it): no CatalogReport in its hello, and a
// request — which a muster that read the hello never sends — gets no answer.
func TestAHostWithNoCatalogAdvertisesNoneAndAnswersNothing(t *testing.T) {
	cfg := Config{Backend: listedBackend{[]string{"main"}}, Catalog: &CatalogScanner{Config: t.TempDir(), Home: t.TempDir(), SnippetsFile: "-"}, NoCatalog: true}
	hello, got := rawMuster(t, cfg, []mirror.Msg{{Type: mirror.TypeCatalog, ID: 1, Session: "main"}}, 700*time.Millisecond)
	if hello.CatalogReport {
		t.Fatal("a NoCatalog host advertises a catalog")
	}
	if len(got) != 0 {
		t.Fatalf("a NoCatalog host answered: %+v", got)
	}
}
