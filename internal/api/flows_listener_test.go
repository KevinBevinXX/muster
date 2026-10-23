package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// TestTheFlowDoorsBehaveTheSameOnBothListeners: the browser-flow JSON API (a
// personal mst_ token only) and the runner door (a runner mfr_ token only) take
// their OWN credential on the machine listener AND the browser listener in
// gotrue mode — the laptop runner reaches muster through the mesh relay to the
// MACHINE port, an off-LAN CLI through the browser one — and refuse the shared
// hook and service tokens on both (#74 refuses them on the browser listener for
// the hook tier; these doors never accepted them anywhere).
func TestTheFlowDoorsBehaveTheSameOnBothListeners(t *testing.T) {
	s, h, seen, _ := newMachineListenerTestServer(t)
	doors := map[authz.Via][]string{}
	for _, p := range routeVias[authz.ViaToken] {
		if strings.Contains(p, "/api/flows/") {
			doors[authz.ViaToken] = append(doors[authz.ViaToken], p)
		}
	}
	doors[authz.ViaRunner] = routeVias[authz.ViaRunner]
	if len(doors[authz.ViaToken]) != 4 || len(doors[authz.ViaRunner]) != 3 {
		t.Fatalf("expected 4 flow API (enqueue, list, read, cancel) and 3 runner routes, got %v", doors)
	}
	listeners := map[string]http.Handler{"machine": h, "browser": BrowserListener(h)}
	for via, patterns := range doors {
		for _, pattern := range patterns {
			method, path := concretePath(pattern)
			for lname, lh := range listeners {
				t.Run(lname+" "+string(via)+" "+pattern, func(t *testing.T) {
					*seen = nil
					req := httptest.NewRequest(method, path, strings.NewReader(""))
					credential(s, req, via)
					rec, aborted := serveRec(t, lh, req)
					want := authz.Principal{UserID: authz.OperatorUserID, Role: authz.RoleAdmin, Via: via}
					if !aborted || len(*seen) != 1 || (*seen)[0] != want {
						t.Fatalf("own credential: status %d body %q saw %v; want %v", rec.Code, rec.Body.String(), *seen, want)
					}
					for _, shared := range []authz.Via{authz.ViaHook, authz.ViaService} {
						*seen = nil
						req := httptest.NewRequest(method, path, strings.NewReader(""))
						credential(s, req, shared)
						rec, aborted := serveRec(t, lh, req)
						if aborted || len(*seen) != 0 || rec.Code < 400 {
							t.Fatalf("the %s token was admitted: status %d saw %v", shared, rec.Code, *seen)
						}
					}
				})
			}
		}
	}
}
