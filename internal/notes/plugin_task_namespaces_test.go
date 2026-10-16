package notes

import (
	"strings"
	"testing"
)

// TestPluginTaskNamespacesBecomeExternalIDNamespaces: a configured namespace is
// accepted by IsPluginTaskNamespace AND kept out of the filter row
// (IsExternalIDTag), a reserved one is refused without changing the set, and nil
// clears it. CONTROL: an unconfigured namespace is neither.
func TestPluginTaskNamespacesBecomeExternalIDNamespaces(t *testing.T) {
	t.Cleanup(func() { _ = SetPluginTaskNamespaces(nil) })
	if IsPluginTaskNamespace("tracker") || IsExternalIDTag("tracker:1") {
		t.Fatal("CONTROL: tracker is a plugin task namespace before it is configured")
	}
	if err := SetPluginTaskNamespaces([]string{"tracker"}); err != nil {
		t.Fatal(err)
	}
	if !IsPluginTaskNamespace("tracker") || !IsExternalIDTag("tracker:1") || IsPluginTaskNamespace("other") {
		t.Fatal("tracker configured: want it a plugin task AND external-id namespace, and other neither")
	}
	if got := ExternalIDNamespaces(); strings.Join(got, ",") != "clickup,superseded-by,tracker" {
		t.Fatalf("ExternalIDNamespaces()=%v; want the closed set plus tracker", got)
	}
	for _, bad := range []string{NSGate, NSAuto, NSInitiative, NSProject, NSSupersededBy, "Tracker", "a/b", ""} {
		if err := SetPluginTaskNamespaces([]string{bad}); err == nil {
			t.Errorf("namespace %q accepted", bad)
		}
	}
	if !IsPluginTaskNamespace("tracker") {
		t.Fatal("a refused Set changed the configured set")
	}
	if err := SetPluginTaskNamespaces(nil); err != nil || IsPluginTaskNamespace("tracker") || len(PluginTaskNamespaces()) != 0 {
		t.Fatal("nil did not clear the set")
	}
}
