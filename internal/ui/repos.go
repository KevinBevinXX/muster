package ui

import (
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// RepoView is the minimal repo shape the UI renders (decoupled from the GitHub
// API client type).
type RepoView struct {
	FullName string // owner/name
	Private  bool
	HTMLURL  string
}

// ReposPanel is the static shell for the Repos tab; it lazy-loads /ui/repos.
func ReposPanel(active bool) g.Node {
	return Div(
		ID(panelID("repos")),
		g.Attr("role", "tabpanel"),
		// destructiveSwap=true: hx-target is this panel and hx-swap is innerHTML, so a
		// refresh discards every node inside it. See panelClass.
		Class(panelClass(active, true)),
		hx("hx-get", "/ui/repos"),
		hx("hx-trigger", "load, github:changed from:body"),
		hx("hx-target", "#"+panelID("repos")),
		hx("hx-swap", "innerHTML"),
	)
}

// ReposNotConfigured is shown when no GitHub OAuth App credentials are set.
func ReposNotConfigured() g.Node {
	return reposCenter("📦", "GitHub not configured",
		"Set GITHUB_CLIENT_ID and GITHUB_CLIENT_SECRET (a GitHub OAuth App) to connect an account.")
}

// ReposUnavailable is shown when the GitHub CONNECTION STORE itself was never
// built, so there is nowhere to read a connection from and nowhere to put one.
//
// 🔴 IT IS A DIFFERENT STATE FROM ReposNotConfigured, AND COLLAPSING THE TWO
// WOULD MISDIRECT THE ONLY READER WHO SEES THIS. ReposNotConfigured means "the
// store is there, nothing is connected, and the OAuth App that would connect one
// is unset" — the remedy is the OAuth App. This one means the store was not
// constructed at all, so the OAuth App would change nothing: the encryption key
// is what decides whether the store exists (a token store with no key would have
// to write tokens in the clear), and without it the connect flow has nothing to
// save into. Telling an operator to set the OAuth App here sends them to fix the
// half that is not missing.
//
// ⚠ IT RENDERS THROUGH reposCenter LIKE ITS THREE SIBLINGS, DELIBERATELY. The
// stylesheet is built from these views and compared byte for byte against the
// committed copy (`make css-check`), so a state that invents its own classes
// makes a copy change a stylesheet change. There is no design reason for this
// state to look unlike the other empty states either.
func ReposUnavailable() g.Node {
	return reposCenter("📦", "GitHub not available",
		"This deployment did not build the GitHub connection store. Set "+
			"MUSTER_GITHUB_ENCRYPTION_KEY (with a database configured) to enable it.")
}

// ReposConnect is shown when OAuth is configured but no account is connected.
func ReposConnect() g.Node {
	return Div(
		Class("mt-16 flex flex-col items-center justify-center gap-4 text-center"),
		Div(Class("text-4xl"), g.Text("📦")),
		P(Class("text-lg font-medium text-fg2"), g.Text("Connect GitHub")),
		P(Class("max-w-xs text-sm text-muted"), g.Text("Authorize muster to list your repos and hand them to agents.")),
		A(
			Href("/github/connect"),
			// Real load (boost off): /github/connect 302-redirects to GitHub's
			// cross-origin OAuth authorize page. A boosted AJAX nav can't body-swap a
			// cross-origin redirect — this must be a full browser navigation.
			hx("hx-boost", "false"),
			Class("press inline-flex items-center gap-2 rounded-xl bg-accent px-4 py-3 text-base font-semibold text-on-accent transition hover:bg-accent/90"),
			g.Raw(`<svg class="h-5 w-5" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38v-1.32c-2.23.49-2.7-1.07-2.7-1.07-.36-.93-.89-1.18-.89-1.18-.73-.5.05-.49.05-.49.81.06 1.23.83 1.23.83.72 1.23 1.88.87 2.34.67.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.83-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.6 7.6 0 0 1 4 0c1.53-1.03 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.52.56.83 1.28.83 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48v2.2c0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8Z"/></svg>`),
			g.Text("Connect GitHub"),
		),
	)
}

// ReposConnected lists the connected account's repos with a disconnect control.
func ReposConnected(login string, repos []RepoView) g.Node {
	return g.Group{
		Div(
			Class("mb-3 flex items-center gap-2"),
			chip("github", login),
			Span(Class("flex-1")),
			Button(
				Type("button"),
				Class("press min-h-[44px] rounded-lg px-2 py-1 text-xs font-medium text-muted transition hover:text-st-error-fg"),
				hx("hx-post", "/github/disconnect"),
				hx("hx-target", "#"+panelID("repos")),
				hx("hx-swap", "innerHTML"),
				hx("hx-confirm", "Disconnect GitHub?"),
				g.Text("Disconnect"),
			),
		),
		reposList(repos),
	}
}

// ReposViaApp lists the repositories a configured GitHub App's installation(s)
// can access: muster's GitHub identity when the App is set. accounts are the
// installation account logins; problem, when non-empty, is the App's current
// operator-facing failure (its credentials refused, a rate limit, a listing
// error) and is shown above the list. No Disconnect: the App is configured by
// the deployment, not connected per user.
func ReposViaApp(accounts []string, repos []RepoView, problem string) g.Node {
	label := strings.Join(accounts, ", ")
	if label == "" {
		label = "no installation"
	}
	return g.Group{
		Div(
			Class("mb-3 flex items-center gap-2"),
			g.Attr("data-github-app", ""),
			chip("github app", label),
		),
		g.If(problem != "", P(g.Attr("role", "alert"), g.Attr("data-github-app-problem", ""),
			Class("mb-3 break-words rounded-lg bg-st-error-bg px-3 py-2 text-xs text-st-error-fg ring-1 ring-inset ring-st-error-fg/40"),
			g.Text(problem))),
		reposList(repos),
	}
}

func reposList(repos []RepoView) g.Node {
	if len(repos) == 0 {
		return reposCenter("📭", "No repositories", "This account has no repositories muster can see.")
	}
	return Div(
		Class("flex flex-col gap-2"),
		g.Map(repos, func(r RepoView) g.Node {
			return Div(
				Class("flex items-center gap-2 rounded-xl border border-line bg-s1/70 px-3 py-2.5 ring-1 ring-line"),
				Span(Class("break-all text-sm font-medium text-fg"), g.Text(r.FullName)),
				g.If(r.Private, Span(Class("rounded-full bg-s2 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide text-muted"), g.Text("private"))),
			)
		}),
	)
}

func reposCenter(icon, title, msg string) g.Node {
	return Div(
		Class("mt-16 flex flex-col items-center justify-center gap-2 text-center"),
		Div(Class("text-4xl"), g.Text(icon)),
		P(Class("text-lg font-medium text-fg2"), g.Text(title)),
		P(Class("max-w-xs text-sm text-muted"), g.Text(msg)),
	)
}
