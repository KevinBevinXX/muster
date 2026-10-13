package flows

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestTheCatalogueIsExactlyMuse pins the closed set. Adding a flow is a
// decision that also needs the runner's local copy (devrc) to list it, so it
// must change this line, not slip in.
func TestTheCatalogueIsExactlyMuse(t *testing.T) {
	if got := strings.Join(Names(), ","); got != "muse.poll,muse.send" {
		t.Fatalf("catalogue = %s; want muse.poll,muse.send", got)
	}
	want := map[string]Class{"muse.poll": ClassRead, "muse.send": ClassWrite}
	for name, class := range want {
		f, ok := Lookup(name)
		if !ok || f.Class != class {
			t.Errorf("%s: class %q ok=%v; want %q", name, f.Class, ok, class)
		}
	}
}

func TestValidateRefusesEverythingOutsideTheSchema(t *testing.T) {
	ok := func() Request {
		return Request{Flow: "muse.send", Host: "laptop", Instance: "personal",
			Params: map[string]string{"text": "hello\nthere", "thread": "Side chat", "task": "42"}}
	}
	cases := map[string]func(r *Request){
		"unknown flow":            func(r *Request) { r.Flow = "browser.nav" },
		"unknown param":           func(r *Request) { r.Params["url"] = "https://x.test" },
		"missing required text":   func(r *Request) { delete(r.Params, "text") },
		"empty required text":     func(r *Request) { r.Params["text"] = "" },
		"text too long":           func(r *Request) { r.Params["text"] = strings.Repeat("é", 4001) },
		"control char in text":    func(r *Request) { r.Params["text"] = "a\x1bb" },
		"text that reads as flag": func(r *Request) { r.Params["text"] = "--force" },
		"text leading dash":       func(r *Request) { r.Params["text"] = "-x" },
		"thread leading dash":     func(r *Request) { r.Params["thread"] = "--force" },
		"thread with newline":     func(r *Request) { r.Params["thread"] = "a\nb" },
		"task not a number":       func(r *Request) { r.Params["task"] = "4; rm" },
		"task zero":               func(r *Request) { r.Params["task"] = "0" },
		"host uppercase":          func(r *Request) { r.Host = "Laptop" },
		"host empty":              func(r *Request) { r.Host = "" },
		"instance with slash":     func(r *Request) { r.Instance = "a/b" },
		"ttl below the minimum":   func(r *Request) { r.TTL = 30 * time.Second },
		"ttl above the maximum":   func(r *Request) { r.TTL = 25 * time.Hour },
		"invalid utf-8":           func(r *Request) { r.Params["text"] = "a\xffb" },
		"poll takes no text":      func(r *Request) { r.Flow = "muse.poll" },
		"thread over 120 runes":   func(r *Request) { r.Params["thread"] = strings.Repeat("t", 121) },
		"instance over 32 chars":  func(r *Request) { r.Instance = strings.Repeat("i", 33) },
		"instance leading hyphen": func(r *Request) { r.Instance = "-x" },
	}
	// Positive control: the unmodified request validates, so each refusal
	// below is caused by its one change.
	r0 := ok()
	if f, err := Validate(&r0); err != nil || f.Name != "muse.send" || r0.TTL != DefaultTTL {
		t.Fatalf("control: the valid request was refused (%v) or not defaulted (ttl %s)", err, r0.TTL)
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := ok()
			mutate(&r)
			if _, err := Validate(&r); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate = %v; want ErrInvalid", err)
			}
		})
	}
}

func TestValidateDropsAnEmptyOptionalParam(t *testing.T) {
	r := Request{Flow: "muse.poll", Host: "laptop", Instance: "personal", Params: map[string]string{"thread": "", "task": "7"}}
	if _, err := Validate(&r); err != nil {
		t.Fatal(err)
	}
	if _, has := r.Params["thread"]; has || r.Params["task"] != "7" {
		t.Fatalf("params = %v; want only task=7", r.Params)
	}
}

func TestRunnerTokenShape(t *testing.T) {
	tok, err := NewRunnerToken()
	if err != nil {
		t.Fatal(err)
	}
	if !RunnerTokenShapeOK(tok) {
		t.Fatalf("a minted token %q fails its own shape check", tok)
	}
	for _, bad := range []string{"", "mst_" + strings.TrimPrefix(tok, RunnerTokenPrefix), tok + "A", RunnerTokenPrefix + strings.Repeat("!", 43)} {
		if RunnerTokenShapeOK(bad) {
			t.Errorf("RunnerTokenShapeOK(%q) = true", bad)
		}
	}
}
