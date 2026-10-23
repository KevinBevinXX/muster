package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
)

// The personal-token door to HUMAN-ONLY approvals (migration 0045):
//
//	POST /api/attention/asks       {title, payload, timeoutSeconds?} → the ask
//	GET  /api/attention/asks/{id}  → the ask (status, answer, approved, consumed)
//
// 🔴 THIS DOOR CAN ASK, NEVER ANSWER. An ask filed here is human_only: only
// the /attention view in a browser session (what the phone's notification
// opens) can answer it — attention_answer from a mirrored session is refused
// by the store and by 0045's CHECK, and no token route answers any ask. So a
// holder of the personal token (an agent on the host that holds it) can raise
// an approval but cannot give one. The approval is then spent by
// POST /api/flows/jobs {"approval": id} on exactly one muse dispatch whose
// text has the ask's payload digest (internal/flows approvalTx).
//
// The ask's asker is muster itself (System), labelled with the token's name,
// so it is listed and notified like any other ask; it has no delivery (a
// system asker's answer is read back here, not pushed into a session).
const attentionAPIPath = "/api/attention/asks"

// attentionAPIMaxTitleRunes bounds the title (the ask's question).
const attentionAPIMaxTitleRunes = 300

func (s *Server) registerAttentionAPIRoutes(mux Mux) {
	mux.HandleFunc("POST "+attentionAPIPath, s.requireAPIToken(s.handleAttentionAskAPI))
	mux.HandleFunc("GET "+attentionAPIPath+"/{id}", s.requireAPIToken(s.handleAttentionGetAPI))
}

type attentionAskBody struct {
	Title          string `json:"title"`
	Payload        string `json:"payload"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

// attentionAskView is the API's view of one ask.
type attentionAskView struct {
	ID            int64  `json:"id"`
	Status        string `json:"status"`
	HumanOnly     bool   `json:"humanOnly"`
	Approved      bool   `json:"approved"`
	Answer        string `json:"answer,omitempty"`
	AnsweredVia   string `json:"answeredVia,omitempty"`
	PayloadSHA256 string `json:"payloadSha256,omitempty"`
	ConsumedJobID int64  `json:"consumedJobId,omitempty"`
	ExpiresAt     string `json:"expiresAt"`
}

func attentionView(a attention.Ask) attentionAskView {
	return attentionAskView{ID: a.ID, Status: string(a.Status), HumanOnly: a.HumanOnly, Approved: a.Approved(),
		Answer: a.Answer, AnsweredVia: a.AnsweredVia.Via, PayloadSHA256: a.PayloadSHA256,
		ConsumedJobID: a.ConsumedJobID, ExpiresAt: a.ExpiresAt.UTC().Format(time.RFC3339)}
}

func (s *Server) handleAttentionAskAPI(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "this server has no attention store (no database)"})
		return
	}
	var b attentionAskBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&b); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body is {\"title\",\"payload\",\"timeoutSeconds\"}"})
		return
	}
	title := strings.TrimSpace(b.Title)
	if title == "" || len([]rune(title)) > attentionAPIMaxTitleRunes {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "title is required, at most " + strconv.Itoa(attentionAPIMaxTitleRunes) + " characters"})
		return
	}
	label := "api token"
	if t, ok := apiTokenFromContext(r.Context()); ok && t.Name != "" {
		label = "api token (" + t.Name + ")"
	}
	a, err := s.ext.Attention.Create(r.Context(), attention.New{
		Asker:    attention.Asker{System: true, Label: label},
		Question: title, HumanOnly: true, Payload: b.Payload,
		Timeout: time.Duration(b.TimeoutSeconds) * time.Second})
	switch {
	case err == nil:
	case errors.Is(err, attention.ErrInvalid), errors.Is(err, attention.ErrTooMany):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": strings.TrimPrefix(err.Error(), "attention: ")})
		return
	default:
		s.logger.Printf("attention: api ask: %v", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the ask could not be filed"})
		return
	}
	// The same two calls as attention_ask: the phone push and the live views.
	s.attentionNotify(a.Owner, askItem(a))
	s.attentionChanged(a.Owner, attentionAskKey(a.ID))
	s.writeJSON(w, http.StatusCreated, attentionView(a))
}

func (s *Server) handleAttentionGetAPI(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "this server has no attention store (no database)"})
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "ask not found"})
		return
	}
	a, err := s.ext.Attention.Get(r.Context(), id)
	switch {
	case err == nil:
		s.writeJSON(w, http.StatusOK, attentionView(a))
	case errors.Is(err, attention.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "ask not found"})
	default:
		s.logger.Printf("attention: api get %d: %v", id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the ask could not be read"})
	}
}
