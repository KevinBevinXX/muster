package hermeshost

import (
	"context"
	"encoding/json"
)

// A PENDING CLARIFY, FROM THE DATABASE (docs/design/hermes.md, "Questions and
// approvals"; Phase 4c). Hermes persists the assistant row that calls the
// clarify tool BEFORE the tool runs (agent/turn_tool_round.py), so a clarify
// waiting on the person is a `clarify` call with no `tool` row answering it
// yet; its arguments are {"questions":[{"question","choices"?,
// "multi_select"?}]}, 1–5 questions of at most 4 choices
// (tools/clarify_tool.py). The answer is the later tool row.
//
// Read only: muster shows the questions, and they are answered in the
// terminal (the viewer's answer reaches only a viewer attached before Hermes
// asked, measured in Phase 4, which attaching on "waiting" never is).

// ClarifyQuestion is one question of a pending clarify.
type ClarifyQuestion struct {
	Question string
	Choices  []string
}

// The bounds a pending clarify is read within (the tool's own).
const (
	maxClarifyQuestions = 5
	maxClarifyChoices   = 4
	clarifyBackfillRows = 64
)

// PendingClarifyOf is the questions of the newest clarify call in rows
// (oldest first) that no tool row answers; nil when there is none.
func PendingClarifyOf(rows []Row) []ClarifyQuestion {
	answered := map[string]bool{}
	for _, r := range rows {
		if r.Role == "tool" && r.ToolCallID != "" {
			answered[r.ToolCallID] = true
		}
	}
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.Role != "assistant" || r.ToolCalls == "" {
			continue
		}
		var calls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		if json.Unmarshal([]byte(r.ToolCalls), &calls) != nil {
			continue
		}
		for j := len(calls) - 1; j >= 0; j-- {
			c := calls[j]
			if c.Function.Name != "clarify" || c.ID == "" || answered[c.ID] {
				continue
			}
			var args struct {
				Questions []struct {
					Question string   `json:"question"`
					Choices  []string `json:"choices"`
				} `json:"questions"`
			}
			if json.Unmarshal([]byte(c.Function.Arguments), &args) != nil {
				return nil
			}
			var out []ClarifyQuestion
			for _, q := range args.Questions {
				if len(out) == maxClarifyQuestions {
					break
				}
				if q.Question == "" {
					continue
				}
				cq := ClarifyQuestion{Question: q.Question}
				for _, ch := range q.Choices {
					if len(cq.Choices) < maxClarifyChoices && ch != "" {
						cq.Choices = append(cq.Choices, ch)
					}
				}
				out = append(out, cq)
			}
			return out
		}
	}
	return nil
}

// PendingClarify is PendingClarifyOf the last rows of root's conversation.
func (s *State) PendingClarify(ctx context.Context, root string) ([]ClarifyQuestion, error) {
	p, err := s.Backfill(ctx, root, clarifyBackfillRows)
	if err != nil {
		return nil, err
	}
	return PendingClarifyOf(p.Rows), nil
}
