package flows

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// The operator's audit of the agent door (#76). The control is the owner's
// approval of the EXACT VISIBLE text; these pin that nothing invisible, and no
// unscreened parameter, can reach the approval card.

// TestAnAgentCannotHideCharactersFromItsApprover: the measured hole was
// "hello" + 29 tag-block characters approved from a card reading `text: hello`.
func TestAnAgentCannotHideCharactersFromItsApprover(t *testing.T) {
	s, _ := pgStore(t)
	agent := seedAgent(t, s, 2, "req-agent-hidden")
	hidden := map[string]string{
		"tag block":            "hello" + strings.Repeat("\U000E0041", 29),
		"tag block unassigned": "hello\U000E0005",
		"zero-width space":     "hel\u200Blo",
		"zero-width joiner":    "hel\u200Dlo",
		"bidi override":        "hello \u202Edlrow",
		"bidi isolate":         "hello \u2066x\u2069",
		"soft hyphen":          "hel\u00ADlo",
		"variation selector":   "hello\uFE0F",
		"private use":          "hello\uE000",
		"word joiner":          "hel\u2060lo",
		"byte order mark":      "hello\uFEFF",
		"line separator":       "hello\u2028there",
		"hangul filler":        "hello\u3164",
		"combining grapheme":   "hel\u034Flo",
		"ogham space":          "hello\u1680there",
	}
	for name, v := range hidden {
		for _, param := range []string{"text", "thread"} {
			r := sendReq("laptop", "personal")
			r.Params[param] = v
			if _, err := s.FileRequest(agentCtx(2), agent, 2, "h", r, ""); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s in %s: FileRequest = %v; want ErrInvalid", name, param, err)
			}
		}
		if _, err := s.FileRequest(agentCtx(2), agent, 2, "h", pollReq("laptop", "personal"), "why "+v); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s in the reason: FileRequest = %v; want ErrInvalid", name, err)
		}
	}
	// NFKC first: full-width digits are an IP address to the screen, and plain
	// full-width letters are stored (and so shown, and sent) as ASCII.
	wide := sendReq("laptop", "personal")
	wide.Params["text"] = "ping \uFF11\uFF19\uFF12.\uFF10.\uFF12.\uFF11\uFF10 now"
	if _, err := s.FileRequest(agentCtx(2), agent, 2, "h", wide, ""); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "IP address") {
		t.Errorf("a full-width IP address = %v; want it screened as an IP address", err)
	}
	ok := sendReq("laptop", "personal")
	ok.Params["text"] = "\uFF48\uFF45\uFF4C\uFF4C\uFF4F caf\u00E9\nsecond line"
	q, err := s.FileRequest(agentCtx(2), agent, 2, "h", ok, "multi\nline   reason")
	if err != nil || q.Params["text"] != "hello caf\u00E9\nsecond line" || q.Reason != "multi line reason" {
		t.Fatalf("control: visible text = %+v %v; want it filed, NFKC-normalised, reason on one line", q, err)
	}
}

// TestEveryParameterAnAgentComposesIsScreened is driven by the CATALOGUE: for
// every flow and every parameter that is not a closed identifier, a value the
// deny screen refuses is refused IN THAT PARAMETER (it used to be `text`
// only, so `thread` carried anything). The identifier set is a ledger.
func TestEveryParameterAnAgentComposesIsScreened(t *testing.T) {
	s, _ := pgStore(t)
	s.WithAgentDenyWords([]string{"bigiron"})
	agent := seedAgent(t, s, 2, "req-agent-params")
	var ids []string
	screened := 0
	for _, f := range Catalogue() {
		for _, p := range f.Params {
			if p.Pattern == taskRE { // the closed identifier (Param.ID; pinned in screen_test.go)
				ids = append(ids, f.Name+"."+p.Name)
				continue
			}
			for _, bad := range []string{"see api.example.com", "ping 192.0.2.10", "ask bigiron"} {
				r := Request{Flow: f.Name, Host: "laptop", Instance: "personal", Params: map[string]string{}}
				for _, other := range f.Params {
					if other.Required {
						r.Params[other.Name] = "fine"
					}
				}
				r.Params[p.Name] = bad
				_, err := s.FileRequest(agentCtx(2), agent, 2, "p", r, "")
				// A patterned parameter (session, marker, wait) refuses these
				// values by its closed shape before the screen sees them; a
				// shape-conforming uuid IS screened (TestAnAgentCannotFileAMuseDispatch).
				// Either way nothing is filed. Free text must meet the screen itself.
				want := "may not send"
				if p.Pattern != nil {
					want = "parameter \"" + p.Name + "\""
				}
				if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), want) {
					t.Errorf("%s.%s = %q: FileRequest = %v; want a refusal containing %q", f.Name, p.Name, bad, err, want)
				}
				screened++
			}
		}
	}
	sort.Strings(ids)
	if got := strings.Join(ids, ","); got != "muse.poll.task,muse.send.task" {
		t.Fatalf("parameters exempt from the text screen = %s; want only the task ids. A new exemption is a decision.", got)
	}
	if screened < 9 {
		t.Fatalf("only %d screened cases ran; the catalogue walk is not walking", screened)
	}
}

func TestRequestsLeaveAnAuditTrailWithoutTheText(t *testing.T) {
	s, pool := pgStore(t)
	sys := authz.System(context.Background())
	agent := seedAgent(t, s, 2, "req-agent-audit")
	const secretText = "the exact words bound for a third party"
	r := sendReq("laptop", "personal")
	r.Params["text"] = secretText
	q, err := s.FileRequest(agentCtx(2), agent, 2, "req-agent-audit", r, "why")
	if err != nil {
		t.Fatal(err)
	}
	_, job, err := s.DecideRequest(session(2), q.ID, true, "owner")
	if err != nil || job == nil {
		t.Fatalf("approve: %v", err)
	}
	q2, err := s.FileRequest(agentCtx(2), agent, 2, "req-agent-audit", pollReq("laptop", "personal"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideRequest(session(2), q2.ID, false, "owner"); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(sys, `SELECT action, via, object, user_id, detail::text FROM audit_log
		WHERE action LIKE 'flow.request.%' AND user_id = 2 ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var action, via, object, detail string
		var uid int64
		if err := rows.Scan(&action, &via, &object, &uid, &detail); err != nil {
			t.Fatal(err)
		}
		got = append(got, action+"|"+via+"|"+object)
		if strings.Contains(detail, secretText) || strings.Contains(detail, "isolation canary") {
			t.Errorf("the audit row for %s carries the parameter TEXT: %s", action, detail)
		}
		var d struct {
			Request int64                     `json:"request"`
			Flow    string                    `json:"flow"`
			Agent   string                    `json:"agent"`
			Job     int64                     `json:"job"`
			Params  map[string]map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(detail), &d); err != nil || d.Flow == "" || d.Agent != "req-agent-audit" || d.Request == 0 {
			t.Errorf("audit detail for %s = %s (%v)", action, detail, err)
		}
		if action == "flow.request.approved" && d.Job != job.ID {
			t.Errorf("the approval's audit row names job %d; want %d", d.Job, job.ID)
		}
		if d.Request == q.ID {
			p := d.Params["text"]
			if p == nil || len(p["sha256"].(string)) != 64 || p["runes"] != float64(len(secretText)) {
				t.Errorf("audit params for the send = %v; want a sha256 and the length %d", d.Params, len(secretText))
			}
		}
	}
	want := []string{
		"flow.request.filed|agent|flow_request:" + itoa(q.ID), "flow.request.approved|session|flow_request:" + itoa(q.ID),
		"flow.request.filed|agent|flow_request:" + itoa(q2.ID), "flow.request.denied|session|flow_request:" + itoa(q2.ID),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit rows:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	// Deleting the agent keeps the request records (agent_id NULL, name kept).
	if _, err := pool.Exec(sys, `DELETE FROM agents WHERE id = $1`, agent); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListRequests(session(2), 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("after the agent was deleted the owner lists %d requests (%v); want both kept", len(list), err)
	}
	var nulled int
	if err := pool.QueryRow(sys, `SELECT count(*) FROM flow_requests WHERE owner_id = 2 AND agent_id IS NULL
		AND agent_name = 'req-agent-audit'`).Scan(&nulled); err != nil || nulled != 2 {
		t.Errorf("%d kept requests have agent_id NULL and the name kept (%v); want 2", nulled, err)
	}
	_, _ = pool.Exec(sys, `DELETE FROM audit_log WHERE action LIKE 'flow.request.%' AND user_id = 2`)
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// TestAnAgentCannotFileAMuseDispatch (round-0 D1): session, marker and wait are
// NOT exempt from the agent-text screen, so an agent-composed thread uuid is
// refused by the long-token rule — the agent door to a dispatch fails closed
// (the dispatch client enqueues with a personal token and never files a
// request). CONTROL: the same request without the uuid is filed, so it is the
// session value, not some other parameter, that the screen refuses.
func TestAnAgentCannotFileAMuseDispatch(t *testing.T) {
	s, _ := pgStore(t)
	agent := seedAgent(t, s, 2, "req-agent-dispatch")
	r := dispatchReq()
	r.Params["text"] = "summarise the reading list"
	delete(r.Params, "task") // a task id the agent's owner may not have; not under test here
	_, err := s.FileRequest(agentCtx(2), agent, 2, "d", r, "dispatch")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "long token-shaped string") {
		t.Fatalf("an agent's dispatch request = %v; want the screen's long-token refusal", err)
	}
	c := dispatchReq()
	c.Params["text"] = "summarise the reading list"
	for _, k := range []string{"task", "session", "marker"} {
		delete(c.Params, k)
	}
	if _, err := s.FileRequest(agentCtx(2), agent, 2, "d", c, "control"); err != nil {
		t.Fatalf("CONTROL: the same request without the uuid was refused: %v", err)
	}
}
