# Session focus

Every session on the Agents list — a dispatched agent or a mirrored Claude Code
window — can carry a **focus**: a short title (stored whole; a card heading longer than 60 characters
shows its first 60 and a **…** button that opens the whole title in the popover,
like a long card name) and one to three sentences saying what it is working on now. A card with a focus is
headed by its focus title, with the card's usual name (the agent's name, or the
mirrored window's title and session) as a subtitle under it. The title is a
button, marked with a chevron: a tap opens a popover with the description and
when it was last updated, and does not open the card. The rest of the card
still opens it. A card with no focus keeps its usual title. The list's filter
box searches focus text too, but ranks matches by the card's **name**, so typing
a name still puts that card first.

Your **agent** reads focus to decide which session should handle something
(`fleet_list`, `focus_search`). It never computes focus itself.

Focus needs a database. It is stored in `agent_focus` (migration `0020`), one
row per session, owned and under row-level security. Its history is in
`agent_focus_history` (migration `0022`); see [focus history](#focus-history).

## What a session is

| kind | keyed by | notes |
|---|---|---|
| `agent` | the agent's id | deleted with the agent |
| `mirror` | the **Claude Code session id** of the conversation in the window | not the tmux window id |
| `mirror` | `hermes:` and the **Hermes session id** (the conversation's lineage root) of a Hermes window | only a Hermes window whose host names its session |

A mirrored session is keyed by the conversation, not the window, so its focus
survives tmux reissuing window ids and follows a conversation into another
window. `muster-mirror` reports the id per window (`claudeSession` in its
window list). An older `muster-mirror` does not, and its windows simply have no
focus. Only Claude Code windows, and Hermes windows whose host names their
session, have one.

The transcript **path** is not sent to the server. The window list is one
WebSocket message, and its largest case already fills 96% of a frame; muster
reads a transcript through the host's own transcript stream instead (below).

## How focus is written

Only muster writes it, server-side. A periodic pass (`MUSTER_FOCUS_INTERVAL`,
default `15m`) and the agent's `focus_refresh` both:
- check the session is one muster can see now — an agent of yours, or a
  Claude session an online window shows. One it cannot see is refused and
  **no row is written**, so naming arbitrary ids cannot grow the table;
- take a **bounded tail** of the session's transcript: at most 40 entries,
  1 KiB each, 16 KiB in all (what each entry carries is below). A dispatched
  agent's tail is its newest chat. A mirrored window's is the backfill the
  host sends when muster opens its transcript stream (the one the rendered
  view opens), closed again at once — and only if that stream is still the
  conversation asked for: after a `/clear` or a restart the window holds
  another conversation, and nothing is stored;
- **skip the summarizer when the tail has not changed**: the tail's sha256
  is stored with the focus, and an equal one makes no call;
- otherwise ask the summarizer (below) for a title and a description, clean
  them (one line, no markup or control characters, three sentences; a title is
  kept whole, cut only past a 300-character storage guard — migration 0023 —
  while the prompt asks for 3 to 8 words), and store them;
- on any failure, keep the previous focus and record **why** beside it, as a
  short fixed reason: `no OpenRouter key`, `the OpenRouter key could not be
  read` (a credential-store failure, not a missing key), `rate-limited`,
  `summarizer error (HTTP <status>)`, `the summarizer's answer was
  unusable`, `the summarizer could not be reached`. Nothing from the transcript, the key or the provider's error
  text is stored or shown. The card's popover says it ("No focus: …", or
  "Last refresh failed: …" on a focus that is now stale, whose title is also
  marked `stale`), and so does
  `fleet_list` (`focus_problem`). The next successful refresh clears it.

A pass only looks at sessions that were active since their focus was
written, at most 20 per user, and skips disabled users. Calls are capped at
60 per user and 300 in all per hour, two at a time.

**Retention.** A sweep runs at startup and then hourly. It runs whether or not
the refresh pass does: with no summarizer, or with `MUSTER_FOCUS_INTERVAL=0`.
It marks every enabled user's visible mirrored sessions as seen, then deletes
every user's mirrored focus not seen for `MUSTER_FOCUS_RETENTION` (default a
week), and every focus history row older than 30 days. A disabled user's sessions are never marked, so their focus ages out.
An agent's focus dies with the agent. `focus_search` never returns a mirrored
session that no online window shows; the store leaves such sessions out before
it applies its result limit.

## Focus history

Each time a session's focus title or description **changes**, the new focus is
also appended to `agent_focus_history`, in the same transaction as the focus
itself. A refresh that produces the same title and description as the newest
history row adds nothing, so a session that is summarized again without moving
does not fill its history.

You can scroll back through it:
- on a mirrored session's rendered view, the "Your messages" sheet (the list
  icon in the header) has two tabs, **Your messages** and **Focus history**.
  The arrow keys move between them, and the tab you pick is remembered for that
  window in the browser;
- on a dispatched agent's page, the target icon in the header opens a
  **Focus history** dialog. That page has no "Your messages" sheet, so the
  history is a dialog of its own rather than a tab.

Both list the newest first, 20 at a time, and load older pages as you scroll
(or with **Load older**). Pages come from `GET /ui/focus/history`, which reads
under your own row-level security and answers 404 for a session that is not
yours (a mirrored session is yours while you hold its history or an online
window of it).

History is kept for **30 days**, whatever happens to the session's focus. A
mirrored session's focus is deleted a week after it was last seen, but its
history stays, and you can still open it. History rows are deleted:
- after 30 days, by the hourly retention sweep (see "Retention" above). The
  row that is a session's current focus is kept past that while the session
  still has a focus, so the history never reads empty beside a focus;
- when an agent is deleted: its history goes with it.

## The summarizer

[ScaleDown](https://docs.scaledown.ai/api-reference/summarize)'s `summarize`
model, through OpenRouter (`scaledown/summarize`). It is called with the
session owner's OpenRouter key, exactly as that owner's gateway agents get it
([configuration](configuration.md)): the environment's for an admin, the
member's own otherwise. An owner with no key gets no summarized focus, and the
error is recorded.

🔴 **What leaves for each kind of session** (to OpenRouter and ScaleDown):

| entry | dispatched agent | mirrored Claude Code session |
|---|---|---|
| your prompts | as written | as written |
| replies | as written | as written (thinking never) |
| tool calls | the tool's **name**, plus the file path when the call names one (`file_path`, `notebook_path`, `path`) | the same |
| tool arguments (shell commands, file bodies, URLs, patterns, anything else) | never | never |
| tool output | never | never |
| errors | — | the error text |

Prompts and replies are sent as they are: whatever you or a session wrote,
including text pasted from elsewhere, goes out. The one exception is a
mirrored session's: its host redacts anything that looks like a secret before
the transcript leaves it ([mirroring](mirroring.md#rendered-and-terminal-views)), so
what reaches the summarizer is redacted too. The prompt asks the summarizer not
to quote commands, secrets or host names, but a dispatched agent's input is not
redacted.

🔴 **Focus is untrusted.** A title or description is a summarizer's output
over a transcript that can carry an outside party's words, so it can repeat an
injected instruction. The summarizer is told to ignore instructions in the
transcript; every focus tool's description and result (`"untrusted": …`) tell
the agent that focus is data, never instructions. Neither makes the text safe
— they only label it.

## The tools

| tool | who | what |
|---|---|---|
| `fleet_list` | agent | every session: id, kind, name, status, focus title, and `focus_problem` (the reason) when its focus is missing or stale |
| `focus_search(query, limit, since_hours?)` | agent | full-text search over titles (weighted higher) and descriptions; id, kind, title and a 120-character snippet per hit. With `since_hours` (1–24) it searches every focus each session had in that window instead of its current one, and each hit carries when that focus was written |
| `focus_refresh(subject)` | agent | one session now (it waits); the periodic pass covers the rest |
| `sessions_overview(limit?)` | agent | every live session (running agents, online mirrored windows) active in the last 24 h, newest-active first: id, kind, name, host, status, current focus, the distinct focus titles of the last 6 h with their times, its pending attention item, and its last three user and assistant messages (300 characters each, secrets redacted, no tool output). A session whose transcript does not arrive within 5 s reads `"transcript": "unavailable (timed out)"`; all the reads share a 12 s budget, and a session it cuts or never reaches reads `"unavailable (overview budget)"`. The answer is capped at 32 KiB and says how many sessions it left out |

All four are on the agent gateway and on `/mcp`. A subject is spelled
`agent:<id>` or `mirror:<claude session id>`, as `fleet_list` and
`focus_search` return it.

The agent uses focus to pick a session for a plugin message it routes; see
[routing a message to a session](plugins.md#routing-a-message-to-a-session).

### Why a time window over history, and not the current focus

A session's focus is rewritten every refresh, so one that has worked a long job
for hours can have a current focus about another subtask of it, and a search of
the current focus misses it. The newest N history rows miss it too: a busy
session writes many rows an hour, so its newest N reach back only as far as its
own churn. A time window is the same span for every session. So the session
hint and `sessions_overview` read **six hours** of history
(`focus.HistoryWindow`), and `focus_search`'s `since_hours` is a window too.
Every one of these reads filters on liveness in SQL before its limit, as the
current-focus search does.
