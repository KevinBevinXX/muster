package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/web"
)

// ---------------------------------------------------------------------------
// THE INSTALLABLE-APP DOCUMENTS: /manifest.webmanifest and /sw.js.
//
// 🔴 NEITHER MAY DEPEND ON A SESSION OR CARRY PER-USER DATA. The public edge
// lets GET/HEAD on exactly these two paths and /static/icons/*.png through
// WITHOUT a session (Chrome's WebAPK icon hasher never sends cookies; without
// the bypass a phone install degrades to a plain shortcut with no share target
// and no shortcuts). Both handlers read nothing from the request, and the
// manifest is a constant built once from constants.
// TestPWAMetadataIsSessionIndependent pins that.
// ---------------------------------------------------------------------------

type manifestImage struct {
	Src        string `json:"src"`
	Sizes      string `json:"sizes"`
	Type       string `json:"type"`
	Purpose    string `json:"purpose,omitempty"`
	FormFactor string `json:"form_factor,omitempty"`
	Label      string `json:"label,omitempty"`
}

type manifestShortcut struct {
	Name      string          `json:"name"`
	ShortName string          `json:"short_name"`
	URL       string          `json:"url"`
	Icons     []manifestImage `json:"icons"`
}

type manifestShareTarget struct {
	Action  string            `json:"action"`
	Method  string            `json:"method"`
	Enctype string            `json:"enctype"`
	Params  map[string]string `json:"params"`
}

// WebManifest is the web app manifest muster serves.
type WebManifest struct {
	ID              string              `json:"id"`
	Name            string              `json:"name"`
	ShortName       string              `json:"short_name"`
	Description     string              `json:"description"`
	StartURL        string              `json:"start_url"`
	Scope           string              `json:"scope"`
	Display         string              `json:"display"`
	ThemeColor      string              `json:"theme_color"`
	BackgroundColor string              `json:"background_color"`
	Icons           []manifestImage     `json:"icons"`
	Shortcuts       []manifestShortcut  `json:"shortcuts"`
	ShareTarget     manifestShareTarget `json:"share_target"`
	Screenshots     []manifestImage     `json:"screenshots"`
}

// Manifest returns the manifest's value.
//
// Design notes, each a decision rather than a default:
//   - id "/" FOREVER. It is the installed app's identity; changing it makes
//     every phone treat the next version as a second app.
//   - no `orientation`. The old portrait lock also locked tablets and foldables.
//   - theme_color/background_color are ui.ThemeColorDark — the same constant the
//     theme-color meta reads, itself pinned to input.css's --mu-bg — so the
//     splash screen, the status bar and the page cannot disagree.
//   - share_target is GET. Behind the forward-auth edge a POST is answered 303
//     with its body dropped; a GET's query survives the login round trip (and
//     muster's own ?next=), so a share started while signed out still lands.
//   - every image lives under /static/icons/, the one static prefix the edge is
//     planned to let through without a session (an Authelia bypass) — the
//     screenshots included, so the richer install sheet does not depend on how
//     the browser fetches them. Hence the fixture-data-only rule on them below.
//   - Chrome on Android shows at most three shortcuts; there are three.
func Manifest() WebManifest {
	icon := func(src, sizes, purpose string) manifestImage {
		return manifestImage{Src: src, Sizes: sizes, Type: "image/png", Purpose: purpose}
	}
	shortcut := func(name, short, url, iconSrc string) manifestShortcut {
		return manifestShortcut{Name: name, ShortName: short, URL: url,
			Icons: []manifestImage{icon(iconSrc, "96x96", "any")}}
	}
	shot := func(src, label string) manifestImage {
		return manifestImage{Src: src, Sizes: "1080x1920", Type: "image/png", FormFactor: "narrow", Label: label}
	}
	return WebManifest{
		ID:        "/",
		Name:      "muster",
		ShortName: "muster",
		Description: "A self-hosted task board for coding agents. Capture a task from anywhere, " +
			"dispatch an agent to work it, review what comes back, and approve the access it asks for.",
		StartURL:        "/",
		Scope:           "/",
		Display:         "standalone",
		ThemeColor:      ui.ThemeColorDark,
		BackgroundColor: ui.ThemeColorDark,
		Icons: []manifestImage{
			icon("/static/icons/icon-192.png", "192x192", "any"),
			icon("/static/icons/icon-512.png", "512x512", "any"),
			icon("/static/icons/maskable-192.png", "192x192", "maskable"),
			icon("/static/icons/maskable-512.png", "512x512", "maskable"),
		},
		Shortcuts: []manifestShortcut{
			shortcut("New task", "New task", "/tasks?new=1", "/static/icons/shortcut-new-96.png"),
			shortcut("Tasks", "Tasks", "/tasks", "/static/icons/shortcut-tasks-96.png"),
			shortcut("Agents", "Agents", "/agents", "/static/icons/shortcut-agents-96.png"),
		},
		ShareTarget: manifestShareTarget{
			Action: "/share",
			Method: "GET",
			// The spec default, stated: Chrome's manifest parser reports a share
			// target without an explicit enctype (measured in the e2e
			// installability spec, as a parse "error" on an otherwise valid file).
			Enctype: "application/x-www-form-urlencoded",
			Params:  map[string]string{"title": "title", "text": "text", "url": "url"},
		},
		// 🔴 These PNGs must only ever be generated from SEEDED FIXTURE DATA
		// (`npm run capture` in e2e/, against the *_e2e seed): /static/icons/* is
		// planned to be reachable without auth, so anything in them is public.
		Screenshots: []manifestImage{
			shot("/static/icons/screenshot-tasks-narrow.png", "The task board, with a task ready for review"),
			shot("/static/icons/screenshot-agents-narrow.png", "Agents at work, each with its live status"),
		},
	}
}

// manifestJSON is the served bytes, built once. A marshal failure of a constant
// struct is a programming error, so it panics at init rather than serving 500s.
var manifestJSON = func() []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(Manifest()); err != nil {
		panic("api: encode manifest: " + err.Error())
	}
	return b.Bytes()
}()

// handleManifest serves the web app manifest from the document root.
func (s *Server) handleManifest(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(manifestJSON)
}

// swBuildPlaceholder is the literal sw.js carries where the build goes.
const swBuildPlaceholder = `"__MUSTER_BUILD__"`

// ServiceWorkerSource is sw.js as served: the embedded file with BuildVersion
// stamped in.
//
// 🔴 THE STAMP IS WHAT MAKES A DEPLOY AN UPDATE. A browser only installs a new
// worker when the script's BYTES change, and most releases change Go and HTML,
// not sw.js — so without this every such deploy left installed apps on the old
// worker and the "new version" prompt never appeared. With it, each build's
// version is part of the script and every deploy is an update.
func ServiceWorkerSource() ([]byte, error) {
	data, err := web.Static.ReadFile("static/sw.js")
	if err != nil {
		return nil, err
	}
	if bytes.Count(data, []byte(swBuildPlaceholder)) != 1 {
		return nil, errSWPlaceholder
	}
	v, _ := json.Marshal(BuildVersion) // a JSON string is a valid JS string literal
	return bytes.Replace(data, []byte(swBuildPlaceholder), v, 1), nil
}

type swPlaceholderError struct{}

func (swPlaceholderError) Error() string {
	return "web/static/sw.js must contain the build placeholder exactly once"
}

var errSWPlaceholder error = swPlaceholderError{}

// handleServiceWorker serves sw.js from the document root so its registration
// scope is "/" (a worker only controls pages at or below its own path).
//
// Cache-Control: no-cache is belt and braces: browsers already bypass the HTTP
// cache for the top-level worker script (updateViaCache defaults to 'imports',
// and pwaScript registers with 'none'). What actually gets an update to a phone
// is (a) the build stamp above changing the bytes and (b) the edge letting this
// path through without a session — with an expired session a worker update is
// a redirect, which the browser treats as a failed update and keeps the old one.
func (s *Server) handleServiceWorker(w http.ResponseWriter, _ *http.Request) {
	data, err := ServiceWorkerSource()
	if err != nil {
		s.logger.Printf("service worker: %v", err)
		http.Error(w, "service worker unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	// Allow the worker to claim the whole origin even though it is fetched from /.
	w.Header().Set("Service-Worker-Allowed", "/")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// ---------------------------------------------------------------------------
// SHARE TARGET AND THE APP BADGE.
// ---------------------------------------------------------------------------

// firstURLRe finds the first http(s) URL in shared text. Android usually puts
// the shared link INSIDE `text` and leaves `url` empty.
var firstURLRe = regexp.MustCompile(`https?://[^\s<>"']+`)

// ShareBody is the task text a share pre-fills: the title, then the text, then
// the URL (or, when the share sent none, the first URL found in the text) — one
// per line, empty parts dropped, and a part already contained in an earlier
// line dropped too, so "text = the URL" does not print the URL twice.
func ShareBody(title, text, link string) string {
	if strings.TrimSpace(link) == "" {
		link = firstURLRe.FindString(text)
	}
	var lines []string
	for _, part := range []string{title, text, link} {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		dup := false
		for _, l := range lines {
			if strings.Contains(l, part) {
				dup = true
				break
			}
		}
		if !dup {
			lines = append(lines, part)
		}
	}
	return ui.ClampComposeBody(strings.Join(lines, "\n"))
}

// handleShare is the manifest's share_target: GET /share?title&text&url opens
// the board with the new-task sheet pre-filled.
//
// 🔴 IT WRITES NOTHING. A GET that created a task would be create-by-link — the
// app's only CSRF defence for a session cookie is SameSite=Lax, which lets a
// top-level GET through. The task is created when the operator presses Save,
// by the existing POST /tasks.
func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	body := ShareBody(q.Get("title"), q.Get("text"), q.Get("url"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := ui.RenderComposePage(w, s.shellFeatures(r.Context()), body); err != nil {
		s.logger.Printf("share: render: %v", err)
	}
}

// handleReviewCount answers GET /ui/tasks/review-count with the number of tasks
// waiting in ready_for_review, for the desktop app badge (pwaScript).
func (s *Server) handleReviewCount(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.ext.Notes == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "this server has no task store"})
		return
	}
	n, err := s.ext.Notes.CountByStatus(r.Context(), notes.StatusReadyForReview)
	if err != nil {
		s.logger.Printf("review count: %v", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not count tasks"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"count": n})
}
