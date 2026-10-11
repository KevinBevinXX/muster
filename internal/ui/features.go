package ui

// Features says which OPTIONAL subsystems the process rendering a document
// actually built. It is what lets a document show a surface only when there is
// something behind it.
//
// 🔴 IT IS A RENDER PARAMETER, NOT PACKAGE STATE, AND THE DIFFERENCE IS
// CORRECTNESS RATHER THAN TASTE. faroConfig is process-wide because telemetry is
// a property of the process; this is a property of the SERVER's extensions, and
// the only honest place to read it is the handler holding that server. A package
// global would also have to be written from a request path — every handler races
// every other one, and a test that renders one shape would decide what a
// concurrent test renders.
//
// 🔴 IT DOES NOT DECIDE WHICH ROUTES EXIST, AND IT MUST NEVER GROW INTO THAT.
// Route registration in internal/api is unconditional on purpose (see that
// package's routes.go): the recorded route set is a function of the code and
// never of a fixture. This type gates what a DOCUMENT draws — a link and a panel
// — while every route stays registered and answers its own unwired case. The two
// are independent claims and both are needed: without the handler's answer a
// stale page still reaches a dead route, and without this a live page mounts a
// panel whose only possible content is a refusal.
type Features struct {
	// GitHub is true when the server holds a GitHub connection store. False
	// removes the Repos tab from the navigation and its panel from the shell.
	//
	// 🔴 FALSE IS THE SHIPPING DEFAULT, NOT AN EDGE CASE. The store is built only
	// when an encryption key is set, so an ordinary deployment renders with this
	// false — which is why the zero value is the unbuilt state rather than the
	// built one. A caller that forgets to populate this draws LESS than it could,
	// which is the safe direction: the opposite default draws a tab over nothing.
	GitHub bool
	// Connectors is true when the server holds a connector store (an encryption
	// key is configured). False removes the Connectors tab and its panel, for the
	// reason GitHub's false removes Repos: the store is built only with a key.
	Connectors bool
}

// visibleTabs is the subset of musterTabs a document with these features may
// draw.
//
// 🔴 EVERY CONSUMER OF THE TAB REGISTRY THAT DRAWS SOMETHING GOES THROUGH HERE —
// the sidebar links, the shell's panels, and the three values navRegistryJS
// hands the browser router. They must agree or the failure is silent in both
// directions: a link with no panel shows nothing when clicked, and a panel with
// no link is unreachable. ui.TabKeys() deliberately does NOT go through here; it
// feeds route registration, which is unconditional.
func visibleTabs(f Features) []tab {
	out := make([]tab, 0, len(musterTabs))
	for _, t := range musterTabs {
		if t.Key == "repos" && !f.GitHub {
			continue
		}
		if t.Key == "connectors" && !f.Connectors {
			continue
		}
		out = append(out, t)
	}
	return out
}

// normalizeVisibleTab clamps an arbitrary tab string to one this document
// actually draws, defaulting to defaultTab.
//
// 🔴 IT IS WHAT MAKES A DEEP LINK TO A HIDDEN TAB LAND SOMEWHERE. Every tab is a
// real route whether or not its subsystem was built, so /repos is served on a
// deployment with no GitHub store. Clamping only against the full registry would
// render the shell with `activeTab = "repos"`, i.e. with no panel un-hidden at
// all: a blank page under a correct-looking URL. Clamping here sends that URL to
// the default tab, which is what an unrouted path already does.
func normalizeVisibleTab(t string, f Features) string {
	for _, vt := range visibleTabs(f) {
		if vt.Key == t {
			return t
		}
	}
	return defaultTab
}
