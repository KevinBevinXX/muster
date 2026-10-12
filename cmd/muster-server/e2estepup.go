//go:build e2eterm

package main

import (
	"net/http"
	"os"
)

// A BROWSER-TEST FIXTURE, COMPILED ONLY WITH `-tags e2eterm`: the step-up gate
// the rendered view's send asks, played. It answers FRESH exactly while the
// file named by envE2EStepUpFreshFile exists, so a spec can show a send
// refused (no file) and then pasted (file created) against one server. Unset,
// it is never fresh.
func init() { e2eMirrorStepUp = fileStepUp{} }

type fileStepUp struct{}

func (fileStepUp) StepUpFresh(*http.Request) bool {
	p := os.Getenv(envE2EStepUpFreshFile)
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}
