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

// Muse-dispatch parameters (session, marker, wait). The fixture values are
// pairwise distinct, and the wait is not the 3600 bound, so a swapped field or
// a hardcoded literal shows.
const (
	dispatchSession = "3f2a9c1e-8b4d-4e7a-9c3b-1d2e3f4a5b6c"
	dispatchMarker  = "md-a1b2c3"
	dispatchWait    = "917"
)

func dispatchReq() Request {
	return Request{Flow: "muse.send", Host: "laptop", Instance: "personal", Params: map[string]string{
		"text": "do the thing", "session": dispatchSession, "marker": dispatchMarker, "wait": dispatchWait, "task": "42"}}
}

func TestValidateAcceptsAMuseDispatch(t *testing.T) {
	r := dispatchReq()
	if _, err := Validate(&r); err != nil {
		t.Fatalf("a well-formed dispatch was refused: %v", err)
	}
	want := map[string]string{"text": "do the thing", "session": dispatchSession, "marker": dispatchMarker, "wait": dispatchWait, "task": "42"}
	if len(r.Params) != len(want) {
		t.Fatalf("params = %v; want %v", r.Params, want)
	}
	for k, v := range want {
		if r.Params[k] != v {
			t.Errorf("param %s = %q; want %q", k, r.Params[k], v)
		}
	}
	// The wait's numeric boundary and a low value are accepted.
	for _, w := range []string{"3600", "1", "60"} {
		r := dispatchReq()
		r.Params["wait"] = w
		if _, err := Validate(&r); err != nil {
			t.Errorf("wait %s refused: %v", w, err)
		}
	}
	// muse.poll takes session (without marker or wait).
	p := Request{Flow: "muse.poll", Host: "laptop", Instance: "personal", Params: map[string]string{"session": dispatchSession, "task": "7"}}
	if _, err := Validate(&p); err != nil || p.Params["session"] != dispatchSession {
		t.Fatalf("muse.poll with a session: %v (params %v)", err, p.Params)
	}
	// A send with neither session nor marker (the pre-dispatch shape) still works.
	plain := Request{Flow: "muse.send", Host: "laptop", Instance: "personal", Params: map[string]string{"text": "hi", "wait": "30"}}
	if _, err := Validate(&plain); err != nil {
		t.Fatalf("a plain send with a wait: %v", err)
	}
}

func TestValidateRefusesMalformedDispatchParams(t *testing.T) {
	cases := map[string]func(r *Request){
		"uppercase uuid":          func(r *Request) { r.Params["session"] = strings.ToUpper(dispatchSession) },
		"short uuid":              func(r *Request) { r.Params["session"] = dispatchSession[:35] },
		"uuid without dashes":     func(r *Request) { r.Params["session"] = strings.ReplaceAll(dispatchSession, "-", "") },
		"uuid with braces":        func(r *Request) { r.Params["session"] = "{" + dispatchSession + "}" },
		"marker 5 hex":            func(r *Request) { r.Params["marker"] = "md-12345" },
		"marker 7 hex":            func(r *Request) { r.Params["marker"] = "md-1234567" },
		"marker uppercase":        func(r *Request) { r.Params["marker"] = "md-ABCDEF" },
		"marker wrong prefix":     func(r *Request) { r.Params["marker"] = "mx-a1b2c3" },
		"wait zero":               func(r *Request) { r.Params["wait"] = "0" },
		"wait over the bound":     func(r *Request) { r.Params["wait"] = "3601" },
		"wait far over the bound": func(r *Request) { r.Params["wait"] = "9999" },
		"wait negative":           func(r *Request) { r.Params["wait"] = "-5" },
		"wait with a unit":        func(r *Request) { r.Params["wait"] = "60s" },
		"wait leading zero":       func(r *Request) { r.Params["wait"] = "060" },
		"session and thread":      func(r *Request) { r.Params["thread"] = "Side chat" },
		"marker without session":  func(r *Request) { delete(r.Params, "session") },
		"marker on muse.poll": func(r *Request) {
			r.Flow = "muse.poll"
			r.Params = map[string]string{"session": dispatchSession, "marker": dispatchMarker}
		},
		"wait on muse.poll": func(r *Request) {
			r.Flow = "muse.poll"
			r.Params = map[string]string{"session": dispatchSession, "wait": dispatchWait}
		},
		"session and thread on muse.poll": func(r *Request) {
			r.Flow = "muse.poll"
			r.Params = map[string]string{"session": dispatchSession, "thread": "Side chat"}
		},
	}
	// Each case names the refusal it must produce, so a case refused for some
	// OTHER reason (an earlier check winning) does not pass as this one.
	wantMsg := map[string]string{
		"session and thread":              `"session" and "thread" are exclusive`,
		"session and thread on muse.poll": `"session" and "thread" are exclusive`,
		"marker without session":          `"marker" needs "session"`,
		"marker on muse.poll":             `takes no parameter "marker"`,
		"wait on muse.poll":               `takes no parameter "wait"`,
		"wait over the bound":             `the limit is 3600`,
		"wait far over the bound":         `the limit is 3600`,
	}
	r0 := dispatchReq()
	if _, err := Validate(&r0); err != nil {
		t.Fatalf("control: the valid dispatch was refused: %v", err)
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := dispatchReq()
			mutate(&r)
			_, err := Validate(&r)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate = %v; want ErrInvalid", err)
			}
			if m := wantMsg[name]; m != "" && !strings.Contains(err.Error(), m) {
				t.Fatalf("Validate = %v; want the refusal %q", err, m)
			}
		})
	}
}
