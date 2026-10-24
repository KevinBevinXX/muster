//go:build e2eterm

package main

import (
	"context"
	"os"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/suggest"
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
	// The suggested replies' model: a fake chat-completions endpoint the spec
	// runs (e2e/tests/suggested-replies.spec.ts), on a fixed invented key, so
	// no owner needs an OpenRouter key and nothing real is ever called.
	e2eSuggestGen = func(model string) suggest.Generator {
		u := os.Getenv(envE2ESuggestURL)
		if u == "" {
			return nil
		}
		return &suggest.OpenRouter{URL: u, Model: model,
			Key: func(context.Context, int64) (string, error) { return "e2e-suggest-key-not-a-real-one", nil }}
	}
}
