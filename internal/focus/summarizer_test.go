package focus

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// completion is a chat completion whose content is content.
func completion(model, content string) string {
	b, _ := json.Marshal(map[string]any{"model": model, "object": "chat.completion",
		"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}})
	return string(b)
}

// scaledownContent is ScaleDown's documented content: a JSON STRING whose
// `summary` field is the text.
func scaledownContent(summary string) string {
	b, _ := json.Marshal(map[string]any{"summary": summary, "input_chars": 900, "output_chars": len(summary),
		"latency_ms": 1200, "input_tokens": 300, "finish_reason": "stop", "notice": nil})
	return string(b)
}

type captured struct {
	auth  string
	body  map[string]any
	calls int
}

func server(t *testing.T, status int, respBody string) (*httptest.Server, *captured) {
	t.Helper()
	c := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.calls++
		c.auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &c.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func client(url string) *ScaleDown {
	return &ScaleDown{URL: url, Key: func(_ context.Context, owner int64) (string, error) {
		if owner == 7 {
			return "owner-seven-fake", nil
		}
		return "", errors.New("no key for this owner")
	}}
}

// TestScaleDownHappyPathJSONStringContent: the documented shape — content is a
// JSON string carrying `summary` — yields the title and description, and the
// request is what the operator decision specifies: the model, the instructions
// as the system message, the tail as the LAST user message, max_tokens within
// 1000, no stream, the owner's key as a bearer.
func TestScaleDownHappyPathJSONStringContent(t *testing.T) {
	srv, c := server(t, 200, completion("scaledown/summarize",
		scaledownContent("Title: Wire session focus into the agent cards\nDescription: Adding a popover to each card. Tests are next.")))
	s, err := client(srv.URL).Summarize(context.Background(), 7, "user: add focus\nassistant: on it")
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "Wire session focus into the agent cards" || s.Description != "Adding a popover to each card. Tests are next." {
		t.Fatalf("summary = %+v", s)
	}
	if s.Model != "scaledown/summarize" {
		t.Errorf("model = %q", s.Model)
	}
	if c.auth != "Bearer owner-seven-fake" {
		t.Errorf("auth = %q", c.auth)
	}
	if c.body["model"] != ScaleDownModel || c.body["stream"] != false {
		t.Errorf("request = %v", c.body)
	}
	if mt, _ := c.body["max_tokens"].(float64); mt <= 0 || mt > 1000 {
		t.Errorf("max_tokens = %v, want 1..1000", c.body["max_tokens"])
	}
	msgs, _ := c.body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v", msgs)
	}
	sys, _ := msgs[0].(map[string]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if sys["role"] != "system" || sys["content"] != SummaryInstructions {
		t.Errorf("first message = %v", sys)
	}
	if last["role"] != "user" || last["content"] != "user: add focus\nassistant: on it" {
		t.Errorf("last message = %v (the tail must be the LAST user message)", last)
	}
}

// TestScaleDownPlainTextContent: a relay that hands the summary through as plain
// text (not ScaleDown's JSON string) still yields a focus.
func TestScaleDownPlainTextContent(t *testing.T) {
	srv, _ := server(t, 200, completion("scaledown/summarize", "Title: Rotate the build key\nDescription: Waiting on review."))
	s, err := client(srv.URL).Summarize(context.Background(), 7, "tail")
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "Rotate the build key" || s.Description != "Waiting on review." {
		t.Fatalf("summary = %+v", s)
	}
}

func TestScaleDownErrorAnswers(t *testing.T) {
	for _, c := range []struct {
		name    string
		status  int
		body    string
		want    error
		status2 int
	}{
		{"429", 429, `{"error":{"message":"rate limited"}}`, ErrRateLimited, 0},
		{"500", 500, `{"error":{"message":"upstream down"}}`, nil, 500},
		{"malformed json", 200, `not json at all`, ErrMalformed, 0},
		{"no choices", 200, `{"choices":[]}`, ErrMalformed, 0},
		{"empty summary", 200, completion("m", scaledownContent("   ")), ErrEmpty, 0},
		{"oversized summary", 200, completion("m", scaledownContent("Title: x\nDescription: "+strings.Repeat("word ", MaxSummaryBytes))), ErrOversized, 0},
		{"oversized body", 200, `{"pad":"` + strings.Repeat("a", maxResponseBytes) + `"}`, ErrOversized, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := server(t, c.status, c.body)
			_, err := client(srv.URL).Summarize(context.Background(), 7, "tail")
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if c.status2 != 0 {
				var he *HTTPError
				if !errors.As(err, &he) || he.Status != c.status2 {
					t.Fatalf("err = %v, want an HTTPError %d", err, c.status2)
				}
			}
		})
	}
}

// TestScaleDownNoKeyCallsNothing: an owner with no OpenRouter key is refused
// before any request leaves.
func TestScaleDownNoKeyCallsNothing(t *testing.T) {
	srv, c := server(t, 200, completion("m", "Title: x"))
	if _, err := client(srv.URL).Summarize(context.Background(), 8, "tail"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("err = %v, want ErrNoKey", err)
	}
	if c.calls != 0 {
		t.Fatalf("%d request(s) were made with no key", c.calls)
	}
	if err := client(srv.URL).CanSummarize(context.Background(), 8); !errors.Is(err, ErrNoKey) {
		t.Fatalf("CanSummarize with no key: %v", err)
	}
	if err := client(srv.URL).CanSummarize(context.Background(), 7); err != nil {
		t.Fatalf("CanSummarize with a key: %v", err)
	}
}

func TestParseSummaryShapes(t *testing.T) {
	for _, c := range []struct{ name, in, title, desc string }{
		{"labelled", "Title: Fix the login loop\nDescription: The cookie expires early. A patch is up.", "Fix the login loop", "The cookie expires early. A patch is up."},
		{"labelled any case, bullets", "- TITLE: Ship 0.4\n* description: Tagging the release.", "Ship 0.4", "Tagging the release."},
		{"unlabelled lines", "Migrate the queue\nMoving rows to 0019. Half done.", "Migrate the queue", "Moving rows to 0019. Half done."},
		{"one line", "Debugging the mirror tunnel. It drops after 30 s.", "Debugging the mirror tunnel", "Debugging the mirror tunnel. It drops after 30 s."},
		{"markdown stripped", "Title: **Bold** `code` title\nDescription: ok", "Bold code title", "ok"},
	} {
		t.Run(c.name, func(t *testing.T) {
			title, desc, err := ParseSummary(c.in)
			if err != nil {
				t.Fatal(err)
			}
			if title != c.title || desc != c.desc {
				t.Fatalf("got (%q, %q), want (%q, %q)", title, desc, c.title, c.desc)
			}
		})
	}
	if _, _, err := ParseSummary("\n \n"); !errors.Is(err, ErrEmpty) {
		t.Fatalf("empty: err = %v, want ErrEmpty", err)
	}
}

// TestCleanClamps: the title is at most MaxTitleRunes, the description at most
// three sentences and MaxDescriptionRunes, control characters and line breaks
// are gone — measured on inputs well past each bound, in multi-byte runes.
func TestCleanClamps(t *testing.T) {
	longTitle := strings.Repeat("é", 300)
	title, _, err := Clean(longTitle, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCountInString(title); n != MaxTitleRunes {
		t.Errorf("title is %d runes, want %d", n, MaxTitleRunes)
	}
	words := strings.TrimSpace(strings.Repeat("token ", 60))
	title, _, _ = Clean(words, "")
	if n := utf8.RuneCountInString(title); n > MaxTitleRunes || !strings.HasSuffix(title, "…") {
		t.Errorf("worded title = %q (%d runes)", title, n)
	}
	_, desc, _ := Clean("t", "One. Two! Three? Four. Five.")
	if desc != "One. Two! Three?" {
		t.Errorf("sentences: %q", desc)
	}
	_, desc, _ = Clean("t", strings.Repeat("ü", 2000))
	if n := utf8.RuneCountInString(desc); n != MaxDescriptionRunes {
		t.Errorf("description is %d runes, want %d", n, MaxDescriptionRunes)
	}
	title, desc, _ = Clean("a\x00b​c\nd", "x\ty\r\nz\x1b[31m")
	if title != "abc d" || desc != "x y z[31m" {
		t.Errorf("control characters: (%q, %q)", title, desc)
	}
	if _, _, err := Clean(" ​ ", "desc"); !errors.Is(err, ErrEmpty) {
		t.Errorf("blank title: err = %v, want ErrEmpty", err)
	}
}

func TestSubjects(t *testing.T) {
	for _, ok := range []string{"agent:12", "mirror:0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b"} {
		s, err := ParseSubject(ok)
		if err != nil || s.String() != ok {
			t.Errorf("%q: %v %v", ok, s, err)
		}
	}
	for _, bad := range []string{"agent:0", "agent:-1", "agent:012", "agent:x", "mirror:", "mirror:@3", "mirror:a/b", "pod:1", "12", "mirror:" + strings.Repeat("a", 65)} {
		if _, err := ParseSubject(bad); !errors.Is(err, ErrBadSubject) {
			t.Errorf("%q parsed", bad)
		}
	}
}
