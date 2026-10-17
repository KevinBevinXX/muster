//go:build e2eterm

package main

import (
	"os"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/github"
)

// A BROWSER-TEST FIXTURE, COMPILED ONLY WITH `-tags e2eterm`: it points the two
// public upstreams the dispatch dialog's pickers read — GitHub's repo list and
// OpenRouter's model catalogue — at fakes the spec runs on loopback
// (e2e/tests/pickers.spec.ts), so the pickers can be driven end to end with
// fixture data. Unset, both stay the real ones.
func init() {
	e2eUpstreamHooks = true
	if u := os.Getenv(envE2EGitHubAPI); u != "" {
		github.UseAPIBaseURL(u)
	}
	if u := os.Getenv(envE2EOpenRouterModels); u != "" {
		api.UseOpenRouterModelsURL(u)
	}
}
