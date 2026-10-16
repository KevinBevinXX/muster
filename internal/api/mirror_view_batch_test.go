package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/ui"
)

// A batch with no readout carries no "context" key at all (what the page
// script sees from an older host, or a batch that did not change it); one with
// a readout carries it under the name the script reads.
func TestAViewBatchCarriesContextOnlyWhenItHasOne(t *testing.T) {
	plain, _ := json.Marshal(mirrorViewBatch{Type: "batch"})
	if strings.Contains(string(plain), "context") {
		t.Errorf("an empty batch carries context: %s", plain)
	}
	with, _ := json.Marshal(mirrorViewBatch{Type: "batch", Context: &ui.ContextView{Tokens: 5, Label: "5 context"}})
	if !strings.Contains(string(with), `"context":{"tokens":5,"label":"5 context"}`) {
		t.Errorf("a batch with a readout: %s", with)
	}
}
