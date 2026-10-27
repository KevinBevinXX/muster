package api

// ---------------------------------------------------------------------------
// SEAMS THIS CHUNK OPENED, CLOSED, OR DELIBERATELY LEFT OPEN.
//
// This file declares nothing. It exists because a seam recorded in a commit
// message is a seam nobody finds, and a seam recorded next to the code it
// affects is one a reader trips over at the right moment. Each entry below
// names WHAT, WHY, the CLOSING CONDITION and WHO CHECKS IT — the four things
// that separate a work item from an object nobody can close.
//
// ---------------------------------------------------------------------------
// 1. THE RECAP GENERATOR IS NOT CARRIED, AND THAT IS A DECISION.
//
//	WHAT: upstream, the tmux grid renders a one-line AI summary of each window.
//	  The FEATURE is the permission router's — the grid, the cache, the
//	  revision rule and the debounce all stay there — but the only GENERATOR is
//	  the dispatched agent, which is muster's. The upstream file that wires them
//	  together therefore splits: everything except `chiefRecapGenerator` and its
//	  prompt stays, and those two would come here.
//	WHY THEY ARE NOT HERE: they would have no caller. The route partition
//	  manifest puts no recap route on muster's side, and the router's own
//	  `POST /api/recaps` seam — the door a generated summary would be delivered
//	  through — records in its header that it has no consumer either. Carrying
//	  the generator now would ship a function nothing calls, reachable through a
//	  route nothing requests, to feed a door nothing pushes to. This repository
//	  has cut exactly that shape twice already (an audit removed three
//	  consumer-less routes from the router before they merged), and the rule it
//	  established is the one applied here.
//	WHY IT IS NOT SIMPLY "DROPPED": the router half still exists and still
//	  expects a generator. Until someone decides, the recap card renders from
//	  cache and goes stale — it does not error, which is exactly why this needs
//	  writing down rather than leaving to be noticed.
//	CLOSING CONDITION: either (a) a pull request adds a muster route that
//	  generates a recap on demand — at which point the generator, its prompt and
//	  the agent-agent lookup move here together and appear in muster's route
//	  golden — or (b) a pull request deletes the router's `POST /api/recaps`
//	  seam and the recap card with it, recording the feature as retired. One or
//	  the other; leaving both halves in place is the state that rots.
//	WHO CHECKS IT: the reviewer of whichever of those two pull requests is
//	  opened, against both projects' route goldens.
//
// ---------------------------------------------------------------------------
// 2. TWO ROUTER ROUTES muster's CREDENTIAL CANNOT REACH.
//
//	WHAT: `POST /api/notify` (push fan-out) is on the router's shared hook
//	  tier; `GET /api/directories` (the picker's data) is behind a browser
//	  session. muster presents a service credential and is refused by both.
//	WHY NOT FIXED HERE: they are routes in another repository. This one cannot
//	  change their auth tier, and holding the hook credential to reach the first
//	  is explicitly refused by the extraction plan — that token also opens task
//	  delete, task status and comment retraction.
//	WHAT IT COSTS MEANWHILE: the agent-ready and task notifications are not
//	  delivered (logged, loudly, at the one fan-out call site); the directory
//	  picker seeds nothing and SAYS SO on screen rather than rendering an empty
//	  dropdown.
//	CLOSING CONDITION: the router registers a service-tier spelling of each, and
//	  internal/router's `Notify` and `Directories` are pointed at them. Both
//	  methods already carry the full note at their own definitions.
//	WHO CHECKS IT: the reviewer of that pull request, against the router's
//	  regenerated route golden — and, for the picker, by opening the create-task
//	  modal and seeing recents rather than the amber line.
//
// ---------------------------------------------------------------------------
// 3. THE BATCH FORM OF THE SESSION LOOKUP IS OWED, AND THIS IS THE MEASUREMENT
//    THAT WAS ASKED FOR.
//
//	WHAT: the extraction plan left one question open at the carve — whether
//	  `GET /api/sessions/{id}` needs a BATCH form, because a board read resolves
//	  N session links and N HTTP round trips is the N+1 the plan refused to
//	  build in-process.
//	WHAT WAS BUILT: the PORT is batched (SessionLivenessProbe.SessionsExisting
//	  takes a slice) and the IMPLEMENTATION fans out per id. So the decorator
//	  already issues ONE call per board read rather than one per card, and a
//	  batch route drops in underneath without touching a single caller.
//	WHY NOT A BATCH ROUTE NOW: it would be a route in the other repository with
//	  no caller until this one is deployed, which is the consumer-less-route cut
//	  again. The plan offered two ways to close this and this is the second:
//	  record the cost rather than build the route.
//	THE COST, STATED AT THE SCOPE IT WAS MEASURED: the plan measured the live
//	  population at 777 session links, 366 of which resolve. A board read
//	  resolves the links on ONE PAGE, not all 777 — the board is paginated and
//	  ListSummaries carries no links at all. ⚠ THIS IS AN ANALYSIS OF THE CODE
//	  PATH, NOT A LATENCY MEASUREMENT: no request has been timed against a real
//	  router, because no deployment of the two services exists yet to time. Do
//	  not quote it as one.
//	CLOSING CONDITION: after the first deployment where both services are live,
//	  time one board read with a populated page and either (a) record the number
//	  here and close this, or (b) add the batch route with this decorator as its
//	  caller. Until one of those happens this stays open.
//	WHO CHECKS IT: whoever performs that first two-service deployment.
//
// ---------------------------------------------------------------------------
// 4. 🔴 THE MOVED HANDLERS ARRIVED WITHOUT THEIR SUITES, AND THE NUMBER IS
//    STATED RATHER THAN LEFT TO BE DISCOVERED.
//
//	WHAT: this package's statement coverage is low, and every test in it was
//	  written FOR the seams, the ports and the guards this carve created —
//	  registration, session liveness, the gate, the directory picker, the
//	  static assets, the actor rule, the thread-search refusal, the
//	  status-writer ledger. The ~10k lines of handler bodies that MOVED here
//	  carry essentially none of their own.
//
//	🔴 THE NUMBER IS A DATED READING, NOT A STANDING FACT, AND THIS PARAGRAPH
//	  SAYS SO BECAUSE THE PREVIOUS ONE DID NOT. It read "this package's
//	  statement coverage is **5.4%**" with nothing deriving it, and by the time
//	  anyone looked it was **7.4%** — moved by two test files added in the same
//	  change that noticed. A hardcoded measurement with no producer does not
//	  stay true; it stays WRITTEN. Re-measure rather than trusting this:
//
//	    MUSTER_TEST_REQUIRE_DB=1 go test -race -cover ./internal/api/
//
//	  Reading: **7.4% of statements, 2026-09-23**, against a live database.
//	  Nothing checks that figure and nothing should — a coverage floor is a
//	  number people raise to whatever passes. What matters is the SHAPE below,
//	  which does not move with the percentage.
//	WHY: upstream those handlers are covered by 122 test files, and they are
//	  not portable as they stand. Measured rather than assumed: 13 of the 122
//	  reference no symbol muster dropped — and all 13 build on a shared fixture
//	  (`newTestServer`) that lives in two files which are themselves
//	  router-coupled, and which construct a Server with a request store, a push
//	  service and a suggest store. muster's Server has none of those and a
//	  different constructor signature. So carrying the suite is not a copy; it
//	  is rebuilding the fixture layer against a different shape, and then
//	  re-porting 122 files onto it.
//	WHY IT IS NOT DONE HERE: it is a chunk, not a step. Doing a third of it
//	  would leave a fixture layer that supports some files and not others,
//	  which is worse than none — the next person cannot tell which failures are
//	  the port and which are real.
//	🔴 WHAT A LOW FIGURE DOES AND DOES NOT MEAN. It does NOT mean the handlers are
//	  untested code paths that never ran: they ran in production upstream for
//	  months. It DOES mean that this repository cannot currently detect a
//	  regression in any of them, and that every guard in this package is a
//	  claim about the seams rather than about the handlers.
//	CLOSING CONDITION: a pull request that ports the fixture layer
//	  (`newTestServer` and friends) onto muster's Server, then moves the
//	  handler suites onto it, and reports this package's coverage after. The
//	  number to beat is the one above; the number to aim at is the upstream
//	  package's.
//	WHO CHECKS IT: the reviewer of that pull request, against a coverage run.
//
// ---------------------------------------------------------------------------
// 5. CLOSED BY THIS CHUNK, RECORDED SO THE CLOSURE IS CHECKABLE.
//
//   - The task-create directory picker was deleted from internal/notes at the
//     start of the carve. It is rebuilt in directories.go, composed across the
//     seam and clamped by the bounds that stayed in internal/notes. (The auth
//     tier it needs is entry 2 above; the composition is done.)
//   - `musterTabs` linked to /runbooks and /privileges, which existed nowhere.
//     Both are registered now, and the route set is DERIVED from ui.TabKeys()
//     rather than typed beside it, so the class of defect cannot recur.
//     TestEveryTabHasADocumentRoute pins it.
//   - Six /static/ paths 404ed. web/ is now an embedded package, `GET /static/`
//     is registered, and the five vendored scripts plus the icons are carried.
//     TestEveryStaticPathTheUIEmitsResolves derives the path set by scanning
//     internal/ui's sources, so a seventh path added to a view fails the test
//     rather than failing silently in a browser.
//
// ---------------------------------------------------------------------------
