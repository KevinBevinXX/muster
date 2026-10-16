package cctranscript

// CONTEXT USAGE: how many tokens of context the conversation fills, as of the
// latest assistant turn.
//
// Context used = that turn's input_tokens + cache_read_input_tokens +
// cache_creation_input_tokens (message.usage): everything the model was SENT
// for the turn. output_tokens is not counted — it is what came back, and it is
// in the next turn's input.
//
// ⚠ A COUNT, NOT A SHARE OF A WINDOW. Which window a session runs in is not in
// an assistant record (a 1M-context session's records still name the bare
// model), so no percentage is derived from one.

// Usage is one assistant turn's context use (Event.Usage).
type Usage struct {
	// Tokens is the context used: input + cache read + cache creation.
	Tokens int64 `json:"tokens"`
}

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
	return &Usage{Tokens: n}
}
