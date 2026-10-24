package suggest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testKey = "sk-or-test-NOTAREALKEY-0123456789"

func completion(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		"usage":   map[string]any{"prompt_tokens": 1234, "completion_tokens": 56, "cost": 0.00015},
	})
	return string(b)
}

// TestGenerateSendsWhatProductionSends: the model, the strict JSON schema, the
// system instructions and the input as the user message, the key only in the
// Authorization header; the answer parsed with its usage.
func TestGenerateSendsWhatProductionSends(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = io.WriteString(w, completion(`{"suggestions":[{"text":"Go with (a)","kind":"pick","cites":["R"],"quote":"(a) checkpoint"}]}`))
	}))
	defer srv.Close()
	o := &OpenRouter{Key: func(context.Context, int64) (string, error) { return testKey, nil }, URL: srv.URL}
	g, err := o.Generate(context.Background(), 1, "THE-INPUT")
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer "+testKey {
		t.Fatalf("auth %q", auth)
	}
	if got["model"] != "openai/gpt-6-luna" { // the literal: a changed default must fail here
		t.Fatalf("model %v", got["model"])
	}
	msgs := got["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != Instructions || msgs[1].(map[string]any)["content"] != "THE-INPUT" {
		t.Fatalf("messages %v", msgs)
	}
	rf := got["response_format"].(map[string]any)
	if rf["type"] != "json_schema" || rf["json_schema"].(map[string]any)["strict"] != true {
		t.Fatalf("response_format %v", rf)
	}
	if r := got["reasoning"].(map[string]any); r["effort"] != DefaultReasoningEffort || r["exclude"] != true {
		t.Fatalf("reasoning %v", r)
	}
	if len(g.Raw) != 1 || g.Raw[0].Kind != KindPick || g.Usage.PromptTokens != 1234 || g.Usage.Cost != 0.00015 {
		t.Fatalf("generation %+v", g)
	}
}

// TestGenerateErrorsNeverCarryTheKey: every failure maps to a fixed reason,
// and no error text holds the key.
func TestGenerateErrorsNeverCarryTheKey(t *testing.T) {
	cases := []struct {
		status int
		body   string
		reason string
	}{
		{429, "slow down", ReasonRateLimited},
		{401, `{"error":"bad key ` + testKey + `"}`, "suggester error (HTTP 401)"},
		{200, `not json`, ReasonUnusable},
		{200, completion(`{"nope":1}`), ReasonUnusable},
		{200, completion("I think you should continue."), ReasonUnusable},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, c.body)
		}))
		o := &OpenRouter{Key: func(context.Context, int64) (string, error) { return testKey, nil }, URL: srv.URL}
		_, err := o.Generate(context.Background(), 1, "x")
		srv.Close()
		if err == nil || Reason(err) != c.reason {
			t.Fatalf("%d %q: err %v reason %q, want %q", c.status, c.body, err, Reason(err), c.reason)
		}
		if strings.Contains(Reason(err), testKey) {
			t.Fatalf("the key is in the reason %q", Reason(err))
		}
	}
	o := &OpenRouter{Key: func(context.Context, int64) (string, error) { return "", nil }}
	if _, err := o.Generate(context.Background(), 1, "x"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
	o = &OpenRouter{Key: func(context.Context, int64) (string, error) { return "", errors.New("db down") }}
	if _, err := o.Generate(context.Background(), 1, "x"); !errors.Is(err, ErrKeyLookup) {
		t.Fatalf("lookup failure: %v", err)
	}
}

// TestParseSuggestionsReadsAFencedAnswerAndCutsToThree.
func TestParseSuggestionsReadsAFencedAnswerAndCutsToThree(t *testing.T) {
	raw, err := ParseSuggestions("```json\n{\"suggestions\":[{\"text\":\"a\"},{\"text\":\"b\"},{\"text\":\"c\"},{\"text\":\"d\"}]}\n```")
	if err != nil || len(raw) != 3 || raw[2].Text != "c" {
		t.Fatalf("%v %v", raw, err)
	}
	if raw, err := ParseSuggestions(`{"suggestions":[]}`); err != nil || len(raw) != 0 {
		t.Fatalf("an empty list is a valid answer: %v %v", raw, err)
	}
}

// FuzzParseSuggestions: whatever the model answers, parsing never panics, and
// whatever survives the filter is well-formed.
func FuzzParseSuggestions(f *testing.F) {
	f.Add(`{"suggestions":[{"text":"Go with (a)","kind":"pick","cites":["R"],"quote":"(a) checkpoint every"}]}`)
	f.Add("```json\n{}\n```")
	f.Add(`{"suggestions":[{"text":"/clear","kind":"redirect","cites":["R"],"quote":"Which do you prefer?"}]}`)
	f.Add(`{"suggestions":null}`)
	b, _ := Build(Input{Tail: []Entry{{Role: RoleAssistant, Text: "(a) checkpoint every 500 rows or (b) re-run. Which do you prefer?"}}})
	f.Fuzz(func(t *testing.T, content string) {
		raw, err := ParseSuggestions(content)
		if err != nil {
			return
		}
		kept, _ := Filter(raw, b)
		if len(kept) > MaxSuggestions {
			t.Fatalf("%d kept", len(kept))
		}
		for _, k := range kept {
			if startsSlashOrBang(k.Text) || strings.ContainsAny(k.Text, "\r\n") || !ValidKind(k.Kind) || k.Text == "" {
				t.Fatalf("a malformed survivor %+v", k)
			}
		}
	})
}
