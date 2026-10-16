# Session focus

Every session on the Agents list — a dispatched agent or a mirrored Claude Code
window — can carry a **focus**: a short title (at most 80 characters) and one to
three sentences saying what it is working on now. Each card shows it as a chip
in its footer; a tap opens a popover with the description and when it was last
updated. The list's filter box searches it too.

Your **chief** reads focus to decide which session should handle something
(`agents_list`, `focus_search`). It never computes focus itself.

Focus needs a database. It is stored in `agent_focus` (migration `0020`), one
row per session, owned and under row-level security.

## What a session is

| kind | keyed by | notes |
|---|---|---|
| `agent` | the agent's id | deleted with the agent |
| `mirror` | the **Claude Code session id** of the conversation in the window | not the tmux window id |

A mirrored session is keyed by the conversation, not the window, so its focus
survives tmux reissuing window ids and follows a conversation into another
window. `muster-mirror` reports the id per window (`claudeSession` in its
window list). An older `muster-mirror` does not, and its windows simply have no
focus. Only Claude Code windows have one.

The transcript **path** is not sent to the server. The window list is one
WebSocket message, and its largest case already fills 96% of a frame; muster
reads a transcript through the host's own transcript stream instead (below).

## How focus is written

1. **By muster, server-side.** A periodic pass (`MUSTER_FOCUS_INTERVAL`,
   default `15m`) and the chief's `focus_refresh` both:
   - take a **bounded tail** of the session's transcript: at most 40 entries
     (prompts, replies, tool calls by name and target), 1 KiB each, 16 KiB in
     all. A dispatched agent's tail is its newest chat. A mirrored window's is
     the backfill the host sends when muster opens its transcript stream, the
     same one the rendered view opens, closed again at once;
   - **skip the summarizer when the tail has not changed**: the tail's sha256
     is stored with the focus, and an equal one makes no call;
   - otherwise ask the summarizer (below) for a title and a description, clean
     them (one line, no markup or control characters, at most 80 characters
     and three sentences), and store them;
   - on any failure, keep the previous focus and record the error beside it.

   A pass only looks at sessions that were active since their focus was
   written, at most 20 per user. Calls are capped at 60 per user and 300 in all
   per hour, two at a time.
2. **By the session itself.** `focus_set(title, description)` sets the calling
   agent's own focus. A summary does not overwrite it for two hours.

## The summarizer

[ScaleDown](https://docs.scaledown.ai/api-reference/summarize)'s `summarize`
model, through OpenRouter (`scaledown/summarize`). It is called with the
session owner's OpenRouter key, exactly as that owner's gateway agents get it
([configuration](configuration.md)): the environment's for an admin, the
member's own otherwise. An owner with no key gets no summarized focus, and the
error is recorded.

🔴 **The tail is sent as-is.** Whatever a session's recent prompts, replies and
tool targets contain goes to OpenRouter and ScaleDown. The prompt asks for no
commands, secrets or host names in the summary, but the input is not redacted.

## The tools

| tool | who | what |
|---|---|---|
| `agents_list` | chief | every session: id, kind, name, status, focus title |
| `focus_search(query, limit)` | chief | full-text search over titles (weighted higher) and descriptions; id, kind, title and a 120-character snippet per hit |
| `focus_refresh(subject?)` | chief | one session now (it waits), or every session that moved (in the background) |
| `focus_set(title, description)` | any agent | its own focus |

All four are on the agent gateway and on `/mcp`. A subject is spelled
`agent:<id>` or `mirror:<claude session id>`, as `agents_list` and
`focus_search` return it.
