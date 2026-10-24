package ui

import "strconv"

// AgentSessionHref is THE in-app link to an agent session's page:
// /agents/{name}?session={id}, the exact URL a bare GET /agents/{name} answers
// with its 303 (api.Server.handleAgentDetail builds its Location with this same
// function, so the two cannot disagree).
//
// 🔴 EVERY IN-APP LINK TO AN AGENT'S PAGE GOES THROUGH HERE, AND IT NAMES THE
// SESSION WHENEVER THE CALLER KNOWS IT. The bare URL costs one extra round trip
// on every open: the browser fetches it, gets the 303, and only then fetches the
// page. Measured with an emulated phone (350 ms RTT, 4x CPU), the first bubble
// landed at 828/1203 ms through the bare link and 476/488 ms through this one.
// api.TestEveryAgentPageLinkGoesThroughAgentSessionHref ledgers every caller and
// fails when a link to the page is built anywhere else.
//
// sessionID <= 0 (the caller has no session: the agent has none yet, or the
// read failed) gives the bare page, which still redirects — and creates the
// session — exactly as before. The bare URL itself keeps redirecting for
// bookmarks and links from outside the app.
func AgentSessionHref(name string, sessionID int64) string {
	if sessionID > 0 {
		return AgentPath(name) + "?session=" + strconv.FormatInt(sessionID, 10)
	}
	return AgentPath(name)
}

// FromListParam is the query parameter (value FromListValue) a LIST link adds to
// AgentSessionHref: "this names whatever was the agent's latest session when
// the list was drawn, not this particular session".
const (
	FromListParam = "from"
	FromListValue = "list"
)

// AgentListHref is a list's link to an agent (a card, a pinned chip, a Recent
// entry, a task card's Open chat): AgentSessionHref on the session that was the
// agent's latest when the list was drawn, marked &from=list.
//
// 🔴 THE MARKER IS WHAT KEEPS THE ONE-HOP LINK FROM GOING STALE. The bare URL
// resolved the latest session at TAP time; a pinned ?session= is the latest at
// DRAW time, and a list can sit open (or in the back/forward cache) while
// another session becomes the latest. api.Server.handleAgentDetail sees the
// marker and, only when the named session is no longer the latest, 303s to the
// latest — so a fresh link costs one hop and a stale one costs the same two the
// bare URL always did. A link WITHOUT the marker (the session drawer, a
// redirect after opening a session) means that session and is never moved.
func AgentListHref(name string, sessionID int64) string {
	h := AgentSessionHref(name, sessionID)
	if sessionID > 0 {
		h += "&" + FromListParam + "=" + FromListValue
	}
	return h
}
