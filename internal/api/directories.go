package api

import (
	"context"
	"github.com/ZacxDev/muster/internal/authz"

	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// THE TASK-CREATE DIRECTORY PICKER — REBUILT ACROSS THE SEAM.
//
// 🔴 THIS FEATURE WAS DELETED FROM internal/notes AT THE START OF THE CARVE AND
// IS BEING PUT BACK HERE, DELIBERATELY, IN A DIFFERENT PLACE.
//
// WHAT IT IS: when you file a task you pick a working directory. The picker
// seeds a short list of recently-used ones and searches the rest as you type.
//
// WHY IT COULD NOT COME WITH internal/notes: the list is derived from the
// permission router's own archive of where work has actually happened. That
// archive is the router's — it is the record of every request it has routed —
// and it stays. A store method reading it would have been a domain package in
// THIS service issuing SQL against ANOTHER service's table, which is precisely
// the one cross-service read the whole extraction exists to delete.
//
// WHY IT WAS NOT SIMPLY DROPPED: the operator decided to keep it. The feature
// is muster's — the create-task modal is muster's surface — and the composition
// is the same shape as session liveness next door: read what is ours locally,
// ask the router for what is theirs, and clamp.
//
// 🔴 THE CLAMPS LIVE IN internal/notes AND ARE APPLIED HERE. notes.DefaultDirectoryLimit
// is the SEED size and is small because every seeded option is rendered into
// the modal's markup on open — it is a payload, not a page size.
// notes.MaxDirectoryResults caps the SEARCH answer, which is what stops a
// one-character query from returning the whole archive. They stay in the domain
// package because a bound a caller merely asks for is a bound the next caller
// can forget; this file is the caller that asks.
//
// ⚠ 🔴 SEAM — IT DOES NOT WORK YET, AND THE REASON IS AN AUTH TIER ON THE OTHER
// SIDE THAT THIS REPOSITORY CANNOT CHANGE.
//
//	WHAT: the router serves GET /api/directories behind a signed OPERATOR
//	  SESSION COOKIE, because a browser was its only caller there. muster
//	  presents a SERVICE credential, which is not a session, so the call is
//	  refused. The router's own source records this in as many words — it lists
//	  the route as the one seam of its four that was deliberately NOT re-tiered,
//	  and gives three reasons, the weightiest being that at the carve the route
//	  would need a new reader on the staying side rather than a different
//	  wrapper. This file is that reader arriving.
//	WHAT HAPPENS MEANWHILE: `failed` comes back true and the modal renders the
//	  amber "could not load recent directories" line. It does NOT render an
//	  empty dropdown. That distinction is the entire reason this function
//	  returns a bool instead of just a slice — see below.
//	CLOSING CONDITION: the router registers a read of that data on its
//	  service tier (the door GET /api/sessions/{id} already uses), and
//	  internal/router's Directories method is pointed at it. Nothing in THIS
//	  file changes when that lands.
//	WHO CHECKS IT: the reviewer of that pull request, against the router's
//	  regenerated route golden, and by opening the create-task modal.
//
// ⚠ AND A HONEST NOTE ON WHETHER IT SHOULD SURVIVE AT ALL: the extraction plan
// measured the table the upstream picker read and found it EMPTY in production
// — zero rows, zero distinct directories — meaning the picker had been silently
// showing nothing since the day it shipped. It queried successfully and
// returned nothing, so no error was ever raised and nobody looked. That is the
// failure this file's `failed` flag exists to make impossible to repeat, and it
// is also a genuine argument that the feature may not be worth the seam. The
// operator decided to rebuild it; that decision is recorded rather than
// re-litigated here.
// ---------------------------------------------------------------------------

// directories reads the picker's options, and reports whether the read FAILED
// as distinct from returning nothing.
//
// 🔴 THE bool IS THE POINT. An empty picker is ALSO what a broken read looks
// like, so without it the two states are indistinguishable on screen and a
// failure has nowhere to surface but a log nobody tails. Non-fatal is still
// right — a missing suggestion list must not block filing a task — but
// non-fatal is not the same as unsaid. The flag rides into the view and renders
// as an amber line.
//
// ⚠ "NO ROUTER CONFIGURED" IS NOT A FAILURE. A standalone deployment has no
// archive to draw on, and there is nothing wrong: the picker is genuinely empty
// and typing a path still works, because the field is free text. Reporting that
// as failed would put a permanent error line on a screen in a configuration
// this service fully supports. A CONFIGURED router that could not be reached IS
// a failure, and is reported as one.
func (s *Server) directories(ctx context.Context, query string, limit int) (dirs []string, failed bool) {
	if s.router == nil {
		return nil, false
	}
	// 🔴 THE ROUTER'S DIRECTORIES ARE THE OPERATOR'S (accounts PR 3b): paths on
	// the operator's own machines, from the operator's own router. A member's
	// session gets none — the same answer as a deployment with no router —
	// rather than a list of somebody else's filesystem. The isolation walk
	// (isolation_test.go) serves a canary path and asserts no member sees it.
	if p, ok := authz.From(ctx); !ok || p.Role != authz.RoleAdmin {
		return nil, false
	}
	dirs, err := s.router.Directories(ctx, query, clampDirectoryLimit(query, limit))
	if err != nil {
		s.logger.Printf("directories: query=%q: %v", query, err)
		return nil, true
	}
	return dirs, false
}

// clampDirectoryLimit bounds what this service will ask another service for.
//
// 🔴 IT CLAMPS ON THE WAY OUT, NOT ONLY ON THE WAY IN, AND THAT IS THE HALF A
// REBUILT-ACROSS-A-SEAM FEATURE LOSES BY DEFAULT. In-process the store honoured
// the bound, so a caller that forgot it got the bound anyway. Across HTTP the
// bound is whatever this client puts in the query string — the other service
// has its own limit, but relying on it means trusting a number nobody here can
// see. A caller asking for 10,000 paths would otherwise get whatever the router
// was willing to send and render every one of them into a modal.
//
// The search cap is the ceiling for ANY query; a blank query additionally
// clamps to the seed size, because a blank query is the modal's own first read
// and that read is a payload.
func clampDirectoryLimit(query string, limit int) int {
	max := notes.MaxDirectoryResults
	if query == "" {
		max = notes.DefaultDirectoryLimit
	}
	if limit <= 0 || limit > max {
		return max
	}
	return limit
}
