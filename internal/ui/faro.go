package ui

import (
	"encoding/json"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// FaroSettings holds the frontend RUM (Grafana Faro) configuration surfaced into
// the page head. It is set ONCE at startup from the environment (see main.go) and
// read by faroHead on every full-document render.
//
// The API key is CLIENT-PUBLIC by design (it is shipped to every browser and only
// gates the public Faro collect endpoint), so it is a plain value, not a secret.
type FaroSettings struct {
	// URL is the Faro collect endpoint (MUSTER_FARO_URL). EMPTY DISABLES all
	// telemetry — no script tags, no init are rendered (the e2e/local default).
	URL string
	// Key is the public Faro app API key (MUSTER_FARO_API_KEY); the SDK sends it
	// as the x-api-key header.
	Key string
	// Version is the running build version, used as the Faro app version.
	Version string
	// Tracing enables the faro-web-tracing bundle + OTEL tracing instrumentation
	// (frontend → Tempo spans). Off by default to keep the phone payload lean.
	Tracing bool
}

// faroConfig is the process-wide Faro config. The zero value (empty URL) renders
// no telemetry, which is exactly the desired local/e2e behavior.
var faroConfig FaroSettings

// SetFaroConfig installs the Faro RUM config read by every page head. Called once
// from main at startup.
func SetFaroConfig(c FaroSettings) { faroConfig = c }

// faroHead returns the Faro <script> tags + inline init for the page head. When
// the collect URL is empty (local/e2e) it returns NOTHING — zero telemetry
// surface, so no SDK loads and no events are sent.
//
// Load order matters: the SDK bundle must execute before the inline init (so
// GrafanaFaroWebSdk is defined) and before the tracing bundle (which depends on
// the SDK global). The vendored bundles are plain IIFEs that synchronously define
// their globals, so they are NOT deferred — they must be ready when the init runs.
func faroHead() g.Node {
	if faroConfig.URL == "" {
		return g.Text("")
	}
	nodes := []g.Node{
		Script(Src("/static/vendor/faro-web-sdk.iife.js")),
	}
	if faroConfig.Tracing {
		nodes = append(nodes, Script(Src("/static/vendor/faro-web-tracing.iife.js")))
	}
	nodes = append(nodes, faroInitScript())
	return g.Group(nodes)
}

// faroInitScript is the inline initializer. All dynamic values are JSON-encoded so
// they are safely embedded regardless of contents. It initializes Faro with the
// standard web instrumentations (page views, web vitals, errors, navigation),
// optionally registers the tracing instrumentation, and exposes window.cgTrack —
// a guarded helper that forwards custom events to faro.api.pushEvent.
//
// The web-vitals instrumentation is among them, so real users' CLS is sent to
// the collector as a `web-vitals` measurement, with the vendored SDK's (1.19)
// default attribution — the element of the largest shift. Nothing in this
// repository reads it back. The browser suite's layout-shift gate,
// e2e/tests/cls.spec.ts, is a separate and stricter number: the sum of every
// shift, not web-vitals' worst session window.
func faroInitScript() g.Node {
	cfg := struct {
		URL     string `json:"url"`
		Key     string `json:"key"`
		Version string `json:"version"`
		Tracing bool   `json:"tracing"`
	}{
		URL:     faroConfig.URL,
		Key:     faroConfig.Key,
		Version: faroConfig.Version,
		Tracing: faroConfig.Tracing,
	}
	// Marshal can only fail on unsupported types; these are all strings/bool.
	b, _ := json.Marshal(cfg)
	return Script(g.Raw(`
(function () {
  // cgTrack is always defined (no-op until/unless Faro initializes) so call sites
  // never have to guard. It forwards a named custom event + attributes to Faro.
  window.cgTrack = function () {};
  try {
    var C = ` + string(b) + `;
    var SDK = window.GrafanaFaroWebSdk;
    if (!SDK || !SDK.initializeFaro || !C.url) return;
    var instrumentations = SDK.getWebInstrumentations();
    if (C.tracing && window.GrafanaFaroWebTracing && window.GrafanaFaroWebTracing.TracingInstrumentation) {
      instrumentations = instrumentations.concat([new window.GrafanaFaroWebTracing.TracingInstrumentation()]);
    }
    var faro = SDK.initializeFaro({
      url: C.url,
      apiKey: C.key,
      app: { name: 'muster', version: C.version || 'dev' },
      instrumentations: instrumentations,
    });
    window.cgTrack = function (name, attrs) {
      try {
        var a = {};
        if (attrs) { for (var k in attrs) { if (Object.prototype.hasOwnProperty.call(attrs, k)) { a[k] = String(attrs[k]); } } }
        faro.api.pushEvent(name, a);
      } catch (e) {}
    };
  } catch (e) {}
})();
`))
}
