# Suggested replies

When a mirrored Claude Code session finishes a turn and hands control back to
you (it asks a question, offers options, or says "should I…"), muster can
offer up to three short replies for the composer, written by a model from what
you asked for, what the session has been working on, and the agent's last
reply. They are labelled **AI-suggested replies**.

🔴 **A suggestion is never sent.** Tapping one fills the composer (below a
draft you already started, never over it); you edit it and press Send, which
keeps its own [step-up check](sending-and-step-up.md). Nothing in the
suggestion path can reach a pane.

On by default; each user can turn them off in **Account → Suggested replies**.
Mirrored tmux sessions only (a dispatched agent's chat page has no chips yet).

## The chip row

- **Where:** a fixed-height row directly **above** the Rendered view's
  composer (the session switcher sits below it). It starts with an **AI**
  mark; the row is a group named "AI-suggested replies", and each chip's
  accessible name is "AI-suggested reply: …".
- **Up to 3 chips**, each a 44px tap target, its text cut with an ellipsis
  (the whole text is its title).
- **A tap fills, never sends.** An empty box takes the chip's text; a box that
  already holds a draft gets it **on a new line below** the draft, never over
  it. It is an edit (Ctrl+Z gives the draft back), and you press Send. A tap
  focuses the box with the caret at the end; a chip chosen from the keyboard
  (Enter, Space) keeps the focus, so a held Enter fills once and sends nothing.
  A held Enter in the box sends once, not once per key repeat.
- **×** hides the row for that turn, on this device; the next turn brings new
  chips.
- **Within 8 s or not at all.** The row takes its room when a set is being
  made (`pending`) and gives it back if no chip is on screen 8 s after the
  turn ended (`ui.SuggestCutoff`); the server does not even call the model
  once that time has passed. A page newly opened on an idle turn that has a
  set shows it (its open reads the whole conversation: a turn end of its own,
  within 8 s of the open). A **reload** resumes its socket and shows **no row**
  for the turn it reloaded into; the next turn brings chips.
- **Gone when the turn is.** A new turn (busy, a new status), a question card
  or an attention ask appearing, or your own Send takes the row away.
- **No layout shift.** The row is out of flow (placed against the pinned
  composer's top), and its 44px are added to the room the page keeps for the
  composer only while a set is pending or shown — so nothing on screen moves
  (measured: CLS 0 at 390×844 and 1280×800, against an in-flow positive
  control the same measurement sees at 0.019 / 0.013).
- Without script there is no row.

## When a set is generated

Only for a mirrored Claude Code session **with a rendered view open**, and only
at the end of a turn: when the host's busy marker falls while the view is open,
when the status's time moves to a non-busy status (a turn that started and
ended between two of the host's looks), or when a view opens on a session that
is already idle. A session nobody is looking at never generates.

Then, every one of these must hold, or nothing is generated (and nothing is
served — the same checks run again whenever the composer asks):

- suggestions are on: not `MUSTER_SUGGEST=off`, and not turned off by you;
- the window is a Claude Code pane that names its session (not Hermes, not a
  plain terminal) and has a rendered view, on a host that is interactive (the
  composer is enabled);
- the window's own status is **idle** — not busy, not **waiting** (a permission
  prompt or a question: text pasted there would answer the prompt), not
  unknown — and not held by the Claude prompt guard (a dialog or text on the
  screen that the status does not show);
- no question card (AskUserQuestion) is open for the session;
- no attention ask raised by the session is open — including one you hid on
  this device with "Hide until next ask": hidden is not answered;
- the final reply hands control back. This pre-gate is a cheap text check (a
  `?` near the end, an option list beside a choice word, or "want me to",
  "should I", "your call", "let me know"…). It is a heuristic, and it only
  decides whether a call is worth making.

A set is served only for the turn it was made for: once the window's status
changes, the old set is no longer offered (and the page takes the row away).

**The turn's own status, by its token.** muster-mirror sends, with the busy
flag on the transcript stream, the time Claude Code wrote that status
(`statusSince`, the same number the window list carries). The marker that
ends a turn therefore names the turn's **own** idle, and the turn end waits
for the window list to read exactly that token — and gives up at once when the
list already reads a later one (the window moved on; that status's own marker
ends its own turn). So:

- a socket that is backed up (the turn already over when its busy marker
  arrives) is judged by its own token, not mistaken for the idle before it;
- two turns back to back with no idle on the stream between them are judged by
  the second turn's token, not by the first turn's idle still in the list.

When the status time moves, the host also re-sends its window list at once
(rather than at its next 5 s refresh), so the wait is short. The wait is
bounded by the 8 s cutoff. A permission prompt or a question **of this turn**
ends it at once (one older than the turn's token is the list lagging, and is
waited out).

🔴 **An older muster-mirror sends no token, and gets no chips at all**:
without the token a turn end cannot be told from the idle before the turn.
The hosts get the new muster-mirror in the same rollout.

## What is sent to the model

Exactly three parts, and nothing else from the session, to OpenRouter on the
session owner's OpenRouter key:

| part | what | caps |
|---|---|---|
| your messages | every prompt you sent in the session, **whole, with its lines**, as the view read its record from the transcript (a prompt older than what the view loaded falls back to the prompt index's first line), **newest first**, numbered `A<n>` by their place in the whole conversation; these are the asks a suggestion cites. Slash-command output and bash-mode (`!cmd`) records are not your messages and are left out | 500 bytes each, 6 KiB in all (the newest kept) |
| the session's focus over time | its [focus history](focus.md#focus-history): each focus's title and description, **oldest first**, with how long ago it was written, in a block marked **untrusted** (a summarizer wrote it) | the last 10 entries, 1 KiB each |
| the transcript tail | the agent's **final reply** (the options or the question), and the 3 entries before it | the final reply 4 KiB, keeping its **end**; the others 1 KiB each |

In the tail, a tool call is its **name** plus the file path when the call
names one. Tool arguments, tool output, thinking and errors are never sent.
Every field goes through the secret redactor (`internal/redact`) on the server
before it leaves, whatever the host already did. A section tag spelled inside
any of it (a message that types `</operator_messages>`) is neutralised, so
content cannot close its section or open another.

## How a suggestion stays grounded

The model must answer strict JSON: up to three items, each with its `text`
(one line, at most 140 characters), its `kind` — `pick` (one of the agent's
options), `gap` (one of your asks the reply did not address), `verify` (a check
the agent skipped), `decide` (approve or decline a proposed action; the model
is asked to give its condition), `redirect` (narrow or stop) — the ids it
`cites` (`R`, the final reply, or an ask `A<n>`) and a `quote` copied from what
it cites.

Every item then passes a deterministic filter, and is dropped unless:

- every cited id exists in the input, and the quote is verbatim (whitespace
  aside) in a cited source and at least 6 bytes long;
- a `pick` and a `decide` cite `R` and quote it — an option or an action must
  be the **agent's own**; a `gap` cites one of your asks;
- **whatever its kind**, a suggestion whose text approves or acts (approve,
  deploy, push, merge, delete, force, run, ship, release, proceed, yes…) cites
  `R` and quotes it. The kind is the model's label and is not trusted to say
  what a suggestion does, so a "redirect" or "verify" that deploys is held to
  the same rule;
- the text shares a word with what it cites, and every identifier-like term in
  it (a `--flag`, a path, a number) appears in the input — a term from
  nowhere, or from text that was never sent (tool output), is dropped;
- it does not start with `/`, `!` or `#` (a command, bash mode or a memory
  note in Claude Code), spans one line, holds no URL and no code fence, and
  the redactor leaves it unchanged.

One heuristic backstop, flagged as such and not a grounding guard: a stock
phrase ("continue", "looks good", "ok"…) is dropped. Nothing checks that a
`decide` carries its condition; the model is asked to.

No survivors means no chips, which is a correct outcome.

⚠ **Residual risk.** The structural defences against prompt injection are that
tool output is never sent, nothing is ever sent automatically, and a tap only
fills. A model that invents an action in plain words that happen to share a
word with the reply can still pass the filter, and so can a bare approval of
the agent's own proposal ("Yes, run it with --force"); the chip is then a
draft you read before sending.

## Cost and caching

A set is cached **in memory only**, by the fingerprint of the exact input
(its focus times aside, which move with the clock) and by the turn it answers:
the same input, or the same turn, never calls the model twice, and a new turn
calls it exactly once. Two views of the same turn share one call, even when
one of them resumed mid-conversation and saw less (a resumed view offers
nothing for the partial turn it resumed into, and generates from the first
whole turn after it). A set's text is dropped after an hour (only its fingerprint is
kept, so an unchanged session is not paid for again). **No suggestion text is
ever written to the database.**

Calls are capped at **120 per user per hour** and 1,000 in all, two at a time,
separately from focus.

Measured on the synthetic eval set (below) with `openai/gpt-6-luna` at
reasoning effort `low`, 64 calls: about 840 tokens in and 160 out per
generation, **$0.00016 per generation**, latency **p50 3.1 s, p95 5.3 s**.

⚠ **Known limit.** A status rewrite while Claude Code stays idle (its
`statusUpdatedAt` moving, for example when a background shell starts or ends —
not measured) counts as a turn end: the chips are redrawn, and if the focus
history moved in between, the model is asked again for the same reply.

## What the chip row reports, and the auto-pause

The row reports, to `POST /mirror/view/suggest/event`, **numbers only**: the
set's fingerprint and `shown` (once per set, and only once it was **seen**: the
page visible and the row in the viewport — a hidden tab or a row off screen is
not an impression), `filled` (a tap) and `sent` (the box was sent after a tap) with the **edit distance** between what the tap left
in the box and what was sent, computed in the browser. A set made for the
**open** of a view on an idle session (not a turn that ended while it was
open) is shown but reported not at all — neither shown nor tapped — so opening
a session to read it never counts toward the auto-pause. The route refuses any
other field, so a page that started sending text would fail, not leak.

**The counts are persisted, numbers only**: `suggest_usage` (migration `0049`)
holds each user's shown, filled and sent counts, the edit-distance sum and the
pause, under forced row-level security. No column can hold text, a session or
any of the conversation; the fingerprints that make a set count once stay in
memory. **Account → Suggested replies** shows them: sets shown, tapped, sent
after a tap, the tap rate, the mean edit before sending, and whether you are
paused.

**Auto-pause:** once 200 sets have been shown to a user, if fewer than 5% of
them were tapped, suggestions pause for that user — nothing is generated or
shown, the composer says so in one line (with a link to Account), and Account
says why. The pause is persisted, so it survives a restart or a deploy.
Turning suggestions back on in Account resumes and starts the count over (a
user who was not paused keeps their counts).

## Turning it off

- `MUSTER_SUGGEST=off` stops everything ([configuration](configuration.md)).
- Each user's own toggle, in **Account → Suggested replies** (or
  `POST /account/suggest` with `on=0` or `on=1`; stored in
  `suggest_preferences`, migration `0048`; on is the default). Off also drops
  that user's cached sets, and a call already running is not kept. A toggle
  that cannot be read as its owner, or not within 5 s after a call, reads as
  off.

## Routes

| route | what |
|---|---|
| `GET /mirror/view/suggest?host=&session=[&window=]` | the window's current set as JSON: `{"ai": true, "label": "AI-suggested replies", "suggestions": [{"text", "kind"}], "fingerprint", "reason"}`. It never generates. `reason` says, in fixed words, why there are none |
| `POST /mirror/view/suggest/event` | the chip row's report: `host`, `session`, `window`, `fingerprint`, `event=shown\|filled\|sent`, `dist` — nothing else is accepted |
| `POST /account/suggest` | `on=1\|0`: the caller's own toggle (`back=account` returns to the Account page) |

The rendered view's socket also tells the page `{"type":"suggest","state":"pending"|"ready"|"none","ms"}`
(`ms`: how long ago the turn ended); the page then reads the set from the GET
route above.

## The eval set

`internal/suggest/testdata/eval/fixtures.yaml` holds 36 **synthetic**
conversations (32 that hand back, 4 that do not; 8 carry a focus history),
several of them injection-shaped, each with the properties a useful set must
have: cite your ask, include the option's label, never contain the injected
action.

- `go test ./internal/suggest/` (every CI run, no key) builds each fixture's
  input exactly as the server does, checks the pre-gate verdict and that no
  tool output or argument reaches the input, and runs each fixture's canned
  answer through the filter.
- `go run ./cmd/muster-suggest-eval -live` (by hand only; it spends the key in
  `MUSTER_AGENT_OPENROUTER_KEY`) runs the real model on every fixture and
  prints each property's pass rate, the latency and the cost. Two runs of all
  32 hand-back fixtures with the default model: 62 of 64 fixture runs met
  every property.
