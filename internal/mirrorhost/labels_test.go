package mirrorhost

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "labels.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadCodenamesReadsAValidFile(t *testing.T) {
	got, skipped, err := LoadCodenames(writeFile(t, `{"scratch13":"mango","scratch2":"Gold"}`))
	if err != nil || len(skipped) != 0 {
		t.Fatalf("err=%v skipped=%v", err, skipped)
	}
	if want := map[string]string{"scratch13": "mango", "scratch2": "Gold"}; !maps.Equal(got, want) {
		t.Fatalf("codenames = %v, want %v", got, want)
	}
}

// TestLoadCodenamesDropsInvalidEntriesNotTheFile: one bad entry costs that
// entry, named, never the rest.
func TestLoadCodenamesDropsInvalidEntriesNotTheFile(t *testing.T) {
	got, skipped, err := LoadCodenames(writeFile(t,
		`{"scratch13":"mango","":"Empty","bad\u001bname":"x","scratch5":"","scratch6":"`+strings.Repeat("y", 65)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"scratch13": "mango"}; !maps.Equal(got, want) {
		t.Fatalf("codenames = %v, want %v", got, want)
	}
	slices.Sort(skipped)
	if want := []string{"", "bad\x1bname", "scratch5", "scratch6"}; !slices.Equal(skipped, want) {
		t.Fatalf("skipped = %q, want %q", skipped, want)
	}
}

func TestLoadCodenamesRefusesAMissingOrMalformedFile(t *testing.T) {
	if _, _, err := LoadCodenames(filepath.Join(t.TempDir(), "absent.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing file: err = %v, want ErrNotExist", err)
	}
	for _, body := range []string{``, `not json`, `null`, `["scratch13","mango"]`, `{"scratch13":1}`, `{"scratch13":"mango"`} {
		got, _, err := LoadCodenames(writeFile(t, body))
		if err == nil || got != nil {
			t.Errorf("body %q: codenames=%v err=%v, want an error and no codenames", body, got, err)
		}
	}
}

func TestParseActiveWindows(t *testing.T) {
	got := parseActiveWindows("1\tscratch13\n8\tscratch2\n0\tname with\ttab-free spaces\nx\tbad\n\tnoindex\n12\t\n3\r\n10\tcrlf\r\n")
	want := map[string]string{"scratch13": "1", "scratch2": "8", "name with\ttab-free spaces": "0", "crlf": "10"}
	if !maps.Equal(got, want) {
		t.Fatalf("parseActiveWindows = %q, want %q", got, want)
	}
}

func TestBuildLabels(t *testing.T) {
	codenames := map[string]string{"scratch13": "mango", "scratch2": "Gold", "gone": "Gone"}
	windows := map[string]string{"scratch13": "1", "scratch2": "8", "infra-work": "2"}
	sessions := []string{"0", "infra-work", "scratch13", "scratch2"}
	got := BuildLabels(codenames, windows, sessions)
	// The operator's own examples; sessions not in the table get nothing (shown
	// by their raw name), and a codename for a session not listed is not sent.
	if want := map[string]string{"scratch13": "mango:1", "scratch2": "Gold:8"}; !maps.Equal(got, want) {
		t.Fatalf("BuildLabels = %v, want %v", got, want)
	}
	// Window unknown: the codename alone.
	if got := BuildLabels(codenames, nil, sessions); !maps.Equal(got, map[string]string{"scratch13": "mango", "scratch2": "Gold"}) {
		t.Fatalf("without windows = %v", got)
	}
	if got := BuildLabels(nil, windows, sessions); got != nil {
		t.Fatalf("no codenames must label nothing, got %v", got)
	}
}

// listOnly is a Backend with no window capability.
type listOnly struct{ countingWindows }

func (listOnly) ActiveWindows() {} // not the interface's signature: no capability

// countingWindows is a Backend that records whether its windows were asked for.
type countingWindows struct {
	asked   *int
	windows map[string]string
	err     error
}

func (c countingWindows) List(context.Context) ([]string, error) { return nil, nil }
func (c countingWindows) Attach(context.Context, string, bool, uint16, uint16) (Terminal, error) {
	return nil, errors.New("no")
}

type withWindows struct{ countingWindows }

func (w withWindows) ActiveWindows(context.Context) (map[string]string, error) {
	*w.asked++
	return w.windows, w.err
}

// wrapped is a wrapper that hides its backend's methods, as NoShrink's does.
type wrapped struct{ Backend }

func (w wrapped) Unwrap() Backend { return w.Backend }

func TestSessionLabelsFindsTheWindowCapabilityThroughAWrapper(t *testing.T) {
	n := 0
	b := withWindows{countingWindows{asked: &n, windows: map[string]string{"scratch13": "5"}}}
	got, err := SessionLabels(context.Background(), wrapped{b}, map[string]string{"scratch13": "mango"}, []string{"scratch13"})
	if err != nil || !maps.Equal(got, map[string]string{"scratch13": "mango:5"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	// Control: the bare type assertion the helper replaces does NOT see it.
	if _, ok := Backend(wrapped{b}).(WindowIndexer); ok {
		t.Fatal("instrument check: the wrapper exposes ActiveWindows directly, so this test proves nothing about Unwrap")
	}
}

func TestSessionLabelsWithoutCodenamesAsksTheBackendNothing(t *testing.T) {
	n := 0
	b := withWindows{countingWindows{asked: &n}}
	if got, err := SessionLabels(context.Background(), b, nil, []string{"scratch13"}); got != nil || err != nil || n != 0 {
		t.Fatalf("got %v, %v, windows asked %d times", got, err, n)
	}
	// Control: with codenames the same backend IS asked.
	_, _ = SessionLabels(context.Background(), b, map[string]string{"scratch13": "mango"}, []string{"scratch13"})
	if n != 1 {
		t.Fatalf("control: windows asked %d times with codenames, want 1", n)
	}
}

func TestSessionLabelsFallsBackToTheCodenameWhenWindowsAreUnknown(t *testing.T) {
	n := 0
	failing := withWindows{countingWindows{asked: &n, err: errors.New("tmux went away")}}
	got, err := SessionLabels(context.Background(), failing, map[string]string{"scratch13": "mango"}, []string{"scratch13"})
	if err == nil || !maps.Equal(got, map[string]string{"scratch13": "mango"}) {
		t.Fatalf("a failed window read: got %v, %v; want the codename alone and the error", got, err)
	}
	got, err = SessionLabels(context.Background(), listOnly{countingWindows{asked: &n}}, map[string]string{"scratch13": "mango"}, []string{"scratch13"})
	if err != nil || !maps.Equal(got, map[string]string{"scratch13": "mango"}) {
		t.Fatalf("a backend with no windows: got %v, %v", got, err)
	}
}
