package main

import (
	"os"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
)

// -list -hermes names the home's gateway: none running, the pin, or a
// gateway muster refuses to attach a viewer to — and it asks identify only.
func TestListHermesChecksTheGatewaysCommit(t *testing.T) {
	withBackend(t, hermesListBackend{})
	home, err := os.MkdirTemp("", "hl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if out := listHermes(t, home, nil); !strings.Contains(out, "gateway\tnone running") {
		t.Fatalf("no gateway:\n%s", out)
	}
	g := hermestest.NewGateway(t, home)
	if out := listHermes(t, home, nil); !strings.Contains(out, "gateway\tcode_sha "+hermeshost.Pin+" (the pin)\t") {
		t.Fatalf("the pin:\n%s", out)
	}
	g.Set("code_sha", "d923a5eb487a17c0a68801bba9ef0f56c81040a8")
	out := listHermes(t, home, nil)
	if !strings.Contains(out, "gateway\tNOT THE PIN\t") || !strings.Contains(out, "d923a5eb487a") || !strings.Contains(out, "answered in the terminal") {
		t.Fatalf("not the pin:\n%s", out)
	}
	if len(g.Tickets) != 0 || len(g.Resumes) != 0 {
		t.Fatal("-list -hermes did more than identify")
	}
}
