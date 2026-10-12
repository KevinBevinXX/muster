package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
)

// --- provisioning the chief agent -----------------------------------------
//
// 🔴 THIS IS THE ONE HANDLER THAT MOVED OUT OF A FILE THAT OTHERWISE STAYED.
// Its former home is the terminal-WRITE door — the agent-identified spelling of
// `send-keys`, with its operator-approval hop — and every other line in that
// file is about a surface muster does not have. This handler is about the AGENT,
// which is muster's, so it travelled alone. The extraction plan names this split
// explicitly, because a whole-file move in either direction would have been
// wrong in a way that compiles.

// handleChiefProvision creates the agent NAMED `chief`, or starts it if it is
// already there. It was written to mirror handleOperatorProvision exactly, because
// chief was the second reserved-name agent and the first already had this shape.
// ⚠ That sibling was deleted in upstream task #653 phase two, so there is nothing
// left to compare it against — do not read this as a live symmetry.
//
// 🔴 IT EXISTS BECAUSE THERE WAS NO WAY TO CREATE ONE. Agent names come from
// agents.BuildUniqueAgentName, an adjective-noun pool (`zesty-stoat`), and the
// dispatch form has no name field — so after 0.8.47 reserved `chief` and branched
// the agent's instructions on that name, NOTHING in the system could produce an
// agent the branch would fire for. The instructions were unreachable and every
// release from 0.8.47 to 0.8.50 was inert for want of this route.
//
// ⚠ IT DOES NOT EXPOSE ARBITRARY NAMING, DELIBERATELY. The obvious alternative —
// a `name` field on the dispatch form — is a wider surface that has to validate a
// k8s-safe slug, guard collisions with the pool, and reject the reserved names it
// must not let a human mint. dispatchParams.Name already existed for exactly this
// case; this route WAS its second caller, not a new capability. ⚠ It is now the
// ONLY one — upstream task #653 phase two deleted handleOperatorProvision, the
// first — and that is what makes `operator` unmintable. agents.go's chatTurn header
// carries the argument and its two pins.
//
// 🔴 IT PROVISIONS THE CALLER'S CHIEF (accounts PR 3b): the reserved slug is
// agents.ChiefNameFor(the principal's user) — `chief` for the operator, as
// before, and `chief-<id>` for anyone else — and the lookup runs on the
// request's context, so under row-level security it can only find the
// caller's own. Another user's chief is invisible here and its slug is not
// the one this request would mint, so a second user's POST creates THEIR
// chief rather than starting (or colliding with) the operator's.
func (s *Server) handleChiefProvision(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := requestOwner(ctx)
	if owner <= 0 {
		http.Error(w, "no user to provision a chief for", http.StatusForbidden)
		return
	}
	chiefName := agents.ChiefNameFor(owner)
	if a, err := s.ext.Agents.GetByName(ctx, chiefName); err == nil {
		// 🔴 safeGo, NOT `go func`, AND THE DIFFERENCE WAS A DEAD CONTAINER. This
		// was the one background goroutine in internal/api written bare while the
		// other seven used safeGo, and a panic in a goroutine with no recover
		// terminates the PROCESS — recoverMiddleware covers the request goroutine
		// only, and by the time this runs the 200 has already been written.
		// Reproduced against the image this tree builds: two POSTs to this route,
		// the second taking THIS branch because the first created the agent, and
		// the container went to `exited (2)` with /health connection-refused. It
		// needed no privilege and no unusual input — click once, nothing appears,
		// click again.
		//
		// ⚠ requireLifecycleProvisioner NOW STOPS THE nil CASE REACHING HERE, AND THIS STILL
		// DOES NOT GO BACK TO BARE. The wrapper answers for one nil field; this
		// recovers whatever a REAL provisioner does on the far side of a helm call
		// to a cluster, which is the case safeGo's own doc names ("any goroutine
		// that touches external data"). The two guards fail for different reasons,
		// so neither substitutes for the other.
		opCtx := context.WithoutCancel(r.Context()) // keeps the principal; see Provisioner
		safeGo(s.logger, "chief start", func() {
			if err := s.ext.Provisioner.Start(opCtx, a.ID); err != nil {
				s.logger.Printf("chief: start: %v", err)
			}
		})
		s.writeJSON(w, http.StatusOK, map[string]any{"name": a.Name, "id": a.ID, "created": false})
		return
	}
	// The model is taken from the request so the operator picks it at provision
	// time; empty falls through to the cluster default the provisioner already
	// applies, exactly as the dispatch form behaves.
	model := strings.TrimSpace(r.FormValue("model"))
	intro := "You are chief, muster's fleet agent. You have no task and no repo, and that is expected. " +
		"Read AGENTS.md, then report the current state of the fleet."
	rec, err := s.createAndDispatchAgent(ctx, dispatchParams{
		// 🔴 THE DISPLAY NAME IS SET EXPLICITLY. Everything that TALKS to the
		// chief (the panel, recaps, threads, `chief ask`) resolves it by display
		// name (chiefAgent), and a dispatch keeps the slug as the display name
		// when none is given. For the operator that was harmless — the slug IS
		// `chief` — but a member's `chief-<id>` would never resolve, so the
		// member's panel said "no agent carries the chief display name" over a
		// chief that existed (accounts PR 3b audit). Same value for user 1.
		Name: chiefName, DisplayName: ChiefDisplayName, Model: model, NoteText: intro, Kickoff: true,
	})
	if err != nil {
		s.logger.Printf("chief: provision: %v", err)
		// This is the route chief was provisioned through with an uncredentialed
		// slug — it has no UI form, so the operator types the model straight into
		// a curl and a generic 500 tells them nothing. Hand back the refusal and
		// its corrected slug.
		if errors.Is(err, agents.ErrModelNotCredentialed) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "could not provision chief", http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"name": rec.Name, "id": rec.ID, "created": true})
}
