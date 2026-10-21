package mirrorhost

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/redact"
	"github.com/ZacxDev/muster/internal/uploads"
)

// REDACTION BEFORE THE RENDERED VIEW'S TRANSCRIPT LEAVES THE HOST.
//
// 🔴 EVERY FREE-TEXT FIELD OF EVERY EVENT A VIEW IS SENT, AND EVERY PROMPT IN
// ITS INDEX, passes redact.Secrets here, on the host, before muster sees it —
// the same redactor (and the same heuristic, with the same limits) the pending
// summary uses (pending.go). A prompt, a reply, an error, a tool call's target
// and input, a tool result's output, a notice, an AskUserQuestion's questions
// and answers: a token pasted into any of them is shown «redacted». What a
// subagent card shows comes from the same events (its result is the Agent
// call's tool_result Output; its fallback description is read out of the
// call's Input), so it is redacted with them.
//
// Each field is capped again afterwards (a replacement can be longer than
// what it replaced): text at MaxText, input at MaxInput, output at MaxOutput,
// a prompt's index line at MaxPromptRefText, and a target at MaxText (it was
// MaxTarget runes, so a redacted one may run a few bytes past that, never past
// the text bound). So every bound the wire's frame budget is reasoned from
// still holds, and BatchLines / BoundPrompts, which run after this, measure
// what is actually sent.
//
// 🔴 AN ATTACHMENT LINE IS KEPT AS MUSTER WROTE IT. A message sent with a file
// carries uploads.AgentLine — muster's own signed link, with its sig= — and
// the view draws that line as the attachment's chip (uploads.FindRefs), never
// showing the link. Redacting its sig= would leave the line unparsable and
// put the half-redacted link in the bubble instead, so the text between
// attachment lines is redacted and the lines themselves are not
// (redactText). The link is one muster minted, expires, and can revoke.
//
// A tool call's Input is JSON (a subagent card decodes its description out of
// it), so it is redacted value by value and stays JSON; one that does not
// decode (cut by CapInput) is redacted as text.

// redactLines redacts every event of lines in place.
func redactLines(lines []cctranscript.Line) {
	for i := range lines {
		for j := range lines[i].Events {
			redactEvent(&lines[i].Events[j])
		}
	}
}

// redactEvent redacts ev's free-text fields in place.
func redactEvent(ev *cctranscript.Event) {
	ev.Text = cctranscript.Cap(redactText(ev.Text), cctranscript.MaxText)
	ev.Target = cctranscript.Cap(redact.Secrets(ev.Target), cctranscript.MaxText)
	ev.Output = cctranscript.Cap(redact.Secrets(ev.Output), cctranscript.MaxOutput)
	ev.Input = redactInput(ev.Input)
	if ev.Ask != nil {
		ev.Ask = redactAsk(ev.Ask)
	}
}

// redactInput is a tool call's input JSON with every string in it redacted,
// still JSON — unchanged when nothing in it looked like a secret — or, when it
// does not decode, redacted as text. Either way within MaxInput.
func redactInput(raw string) string {
	if raw == "" {
		return raw
	}
	d := json.NewDecoder(bytes.NewReader([]byte(raw)))
	d.UseNumber() // numbers are kept as written
	var v any
	if d.Decode(&v) != nil || d.More() {
		return cctranscript.Cap(redact.Secrets(raw), cctranscript.MaxInput)
	}
	v, changed := redactValue(v)
	if !changed {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return cctranscript.Cap(redact.Secrets(raw), cctranscript.MaxInput)
	}
	return cctranscript.CapInput(string(b))
}

// redactValue is v (decoded JSON) with every string redacted, and whether any
// changed.
func redactValue(v any) (any, bool) {
	switch x := v.(type) {
	case string:
		r := redact.Secrets(x)
		return r, r != x
	case map[string]any:
		changed := false
		for k, e := range x {
			if r, c := redactValue(e); c {
				x[k], changed = r, true
			}
		}
		return x, changed
	case []any:
		changed := false
		for i, e := range x {
			if r, c := redactValue(e); c {
				x[i], changed = r, true
			}
		}
		return x, changed
	}
	return v, false
}

// redactAsk is a copy of a with its questions, headers, option labels and
// descriptions and its answers (keyed by question, so the keys too) redacted.
func redactAsk(a *cctranscript.Ask) *cctranscript.Ask {
	out := &cctranscript.Ask{}
	for _, q := range a.Questions {
		rq := askq.Question{Question: redact.Secrets(q.Question), Header: redact.Secrets(q.Header), MultiSelect: q.MultiSelect}
		for _, o := range q.Options {
			rq.Options = append(rq.Options, askq.Option{Label: redact.Secrets(o.Label), Description: redact.Secrets(o.Description)})
		}
		out.Questions = append(out.Questions, rq)
	}
	if a.Answers != nil {
		out.Answers = make(map[string]string, len(a.Answers))
		for k, v := range a.Answers {
			out.Answers[redact.Secrets(k)] = redact.Secrets(v)
		}
	}
	return out
}

// redactPrompts redacts the prompt index's texts in place.
func redactPrompts(refs []cctranscript.PromptRef) {
	for i := range refs {
		refs[i].Text = cctranscript.Cap(redactText(refs[i].Text), cctranscript.MaxPromptRefText)
	}
}

// redactText is redact.Secrets over s except its attachment lines
// (uploads.FindRefs) that are exactly muster's own (keptAttachment), which are
// kept as they are.
func redactText(s string) string {
	refs := uploads.FindRefs(s)
	if len(refs) == 0 {
		return redact.Secrets(s)
	}
	var b strings.Builder
	at := 0
	for _, r := range refs {
		b.WriteString(redact.Secrets(s[at:r.Start]))
		if line := s[r.Start:r.End]; keptAttachment(line, r) {
			b.WriteString(line)
		} else {
			b.WriteString(redact.Secrets(line))
		}
		at = r.End
	}
	b.WriteString(redact.Secrets(s[at:]))
	return b.String()
}

// keptAttachment reports whether an attachment line FindRefs matched is exempt
// from redaction: the line AgentLine writes for its own fields, character for
// character, and nothing in it the redactor would take except the link's sig.
//
// 🔴 THE PARSER'S PATTERN IS WIDER THAN MUSTER'S OWN LINE (#147). It accepts any
// text in the curl command's quotes and any host and path before /f/, so a
// line shaped like an attachment could carry a credential (in the quotes, in a
// user:token@ or a path segment of the link) past the redactor. So the line
// must be AgentLine's (the curl quotes hold the same link, the same name), the
// link carries no userinfo, and with its sig removed the redactor changes
// nothing. Anything else is redacted like the text around it.
func keptAttachment(line string, r uploads.Ref) bool {
	if line != "[attached: "+r.Name+" ("+r.ContentType+", "+r.Size+") "+r.URL+
		" — download with curl -sSfo "+r.Name+" '"+r.URL+"']" {
		return false
	}
	u, err := url.Parse(r.URL)
	if err != nil || u.User != nil || u.Fragment != "" || u.Host == "" {
		return false
	}
	sig := u.Query().Get("sig")
	if sig == "" {
		return false
	}
	masked := strings.ReplaceAll(line, "&sig="+sig, "")
	return redact.Secrets(masked) == masked
}
