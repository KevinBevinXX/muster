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

🔴 **Not generated yet.** This is the server side: the input, the model call,
the filter, the cache, the exclusions, the routes and the per-user toggle. The
turn-end trigger is not wired into the rendered view, so no set is generated
and nothing is spent until the composer's chip row ships with it. On by
default from then on.

## When a set is generated (once wired)

Only for a mirrored Claude Code session **with a rendered view open**, and only
at the end of a turn: when the host's busy marker falls while the view is open,
or when a view opens on a session that is already idle. A session nobody is
looking at never generates.

Then, every one of these must hold, or nothing is generated (and nothing is
served — the same checks run again whenever the composer asks):

- suggestions are on: not `MUSTER_SUGGEST=off`, and not turned off by you;
- the window is a Claude Code pane that names its session (not Hermes, not a
  plain terminal) and has a rendered view, on a host that is interactive (the
  composer is enabled);
- the window's own status is **idle** — not busy, not **waiting** (a permission
  prompt or a question: text pasted there would answer the prompt), not
  unknown;
- no question card (AskUserQuestion) is open for the session;
- no attention ask raised by the session is open — including one you hid on
  this device with "Hide until next ask": hidden is not answered;
- the final reply hands control back. This pre-gate is a cheap text check (a
  `?` near the end, an option list beside a choice word, or "want me to",
  "should I", "your call", "let me know"…). It is a heuristic, and it only
  decides whether a call is worth making.

A set is served only for the turn it was made for: once the window's status
changes, the old set is no longer offered. The window's status can trail the
transcript by a few seconds, so a turn end waits up to 12 s (two of the host's
window-list refreshes) for the turn's own idle status — not the idle from
before the turn. A permission prompt or a question ends the wait at once, and
a turn that moves on while the checks run generates nothing.

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
one of them resumed mid-conversation and saw less (a resumed view never
generates from the partial turn it resumed into; it does from the first whole
turn after it). A set's text is dropped after an hour (only its fingerprint is
kept, so an unchanged session is not paid for again). **No suggestion text is
ever written to the database.**

Calls are capped at **120 per user per hour** and 1,000 in all, two at a time,
separately from focus.

Measured on the synthetic eval set (below) with `openai/gpt-6-luna` at
reasoning effort `low`, 64 calls: about 840 tokens in and 160 out per
generation, **$0.00016 per generation**, latency **p50 3.1 s, p95 5.3 s**.

## Turning it off

- `MUSTER_SUGGEST=off` stops everything ([configuration](configuration.md)).
- Each user's own toggle: `POST /account/suggest` with `on=0` or `on=1`
  (stored in `suggest_preferences`, migration `0048`; on is the default). Off
  also drops that user's cached sets, and a call already running is not
  kept. A toggle that cannot be read as its owner, or not within 5 s after a
  call, reads as off.

An automatic pause for users who rarely use the chips is planned with the
composer that reports which ones were tapped.

## Routes

| route | what |
|---|---|
| `GET /mirror/view/suggest?host=&session=[&window=]` | the window's current set as JSON: `{"ai": true, "label": "AI-suggested replies", "suggestions": [{"text", "kind"}], "fingerprint", "reason"}`. It never generates. `reason` says, in fixed words, why there are none |
| `POST /account/suggest` | `on=1\|0`: the caller's own toggle |

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
