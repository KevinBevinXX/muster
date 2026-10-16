package cctranscript

import "strings"

// CONTEXT USAGE: how much of the model's context window the conversation
// fills, as of the latest assistant turn.
//
// Context used = that turn's input_tokens + cache_read_input_tokens +
// cache_creation_input_tokens (message.usage): everything the model was SENT
// for the turn. output_tokens is not counted — it is what came back, and it is
// in the next turn's input.
//
// 🔴 THE WINDOW IS NOT GUESSED. Which window a session runs in is not in
// message.model: a 1M-context session's assistant records still say
// "claude-opus-5-5" (measured over 832 real transcripts: the largest context
// recorded under a bare "claude-opus-5" was 966,483 tokens, so its records
// cannot be telling 200k from 1M). The ONE record that does say is the
// session's model attachment, {"type":"attachment","attachment":{"type":
// "model","identity":{"modelId":"claude-opus-5-5[1m]"}}}, which the CLI writes
// when a session starts or changes model (195 of those 832 transcripts carry
// one). So:
//
//   - a model id ending in "[1m]" (from that attachment) is a 1M window;
//   - a model in contextWindows is the window listed there — and only models
//     with ONE window are listed;
//   - anything else — a model with a 1M variant and no attachment saying which
//     (every current Opus, Sonnet and Fable), or a model this table does not
//     know — is an unknown window, and only the token count is shown.
//
// An attachment written before the backfill window starts is not seen, so a
// long session can show tokens only until its model next changes. That is the
// honest answer: the transcript the viewer was sent does not say.

// Usage is one assistant turn's context use (Event.Usage).
type Usage struct {
	// Model is the turn's message.model as the CLI wrote it.
	Model string `json:"model,omitempty"`
	// Tokens is the context used: input + cache read + cache creation.
	Tokens int64 `json:"tokens"`
}

// million is the 1M window. oneMillionSuffix marks a model id that runs in it.
const (
	million          = 1_000_000
	oneMillionSuffix = "[1m]"
)

// contextWindows maps a model (by id prefix, so a dated snapshot id matches its
// family) to its window — ONLY for models that have exactly one. Opus 4.6+,
// Sonnet 4.x/5.x and Fable are absent on purpose: each has a 1M variant, and
// their records do not say which one ran (see above).
var contextWindows = []struct {
	prefix string
	window int64
}{
	{"claude-haiku-4-5", 200_000},
	{"claude-3-5-haiku", 200_000},
	{"claude-3-5-sonnet", 200_000},
	{"claude-3-7-sonnet", 200_000},
	{"claude-3-opus", 200_000},
	{"claude-3-haiku", 200_000},
}

// ContextWindow is model's window in tokens, and whether it is known at all.
func ContextWindow(model string) (int64, bool) {
	if strings.HasSuffix(model, oneMillionSuffix) {
		return million, true
	}
	for _, w := range contextWindows {
		if strings.HasPrefix(model, w.prefix) {
			return w.window, true
		}
	}
	return 0, false
}

// BaseModel is a model id without its window marker ("claude-opus-5-5[1m]" →
// "claude-opus-5-5"): what an assistant record's message.model says for it.
func BaseModel(id string) string { return strings.TrimSuffix(id, oneMillionSuffix) }

// usageOf is an assistant record's context use, or nil when it has none worth
// showing: no usage block, an API failure, a synthetic record, or zero tokens.
func usageOf(r Record) *Usage {
	u := r.Message.Usage
	if u == nil || r.IsAPIError || r.Message.Model == "<synthetic>" {
		return nil
	}
	n := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	if n <= 0 {
		return nil
	}
	return &Usage{Model: r.Message.Model, Tokens: n}
}
