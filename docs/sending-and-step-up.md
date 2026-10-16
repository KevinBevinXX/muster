# The message box and step-up

## Where the message box appears

| view | message goes to | needs |
|---|---|---|
| A mirrored window's **Rendered** view (Claude Code) | the Claude Code pane, pasted as typed | the host's `-interactive`, plus a fresh sign-in |
| A mirrored window's **Terminal** view (any program) | the window's active pane, followed by Enter | the host's `-interactive`, plus a fresh sign-in |
| A dispatched agent's chat and **Terminal** view | `POST /agents/<name>/send`: one agent turn, run in the background | a running agent with chat wired ([provisioner.md](provisioner.md)) |
| The chief panel | the chief agent | as above |

Each box can also have a microphone; see [dictation.md](dictation.md).

**Mirrored sends:**

- A message is pasted into a *pane id*, never by switching windows.
- On the Rendered view, it goes only into a pane that is **still** running Claude
  Code, never into a shell left behind after Claude exits. A leading `/` or `!`
  is passed through as typed.
- On the Terminal view, the box does not need the Live keys switch. That switch
  is a live keyboard; the box is one gated paste per Send. So the page never
  calls itself "read-only" while its box can send: with live keys off the status
  reads just "Connected". A read-only host says so once, in the box's disabled
  note.
- Only newline and tab are accepted as control characters. Several lines arrive
  as one bracketed paste, and the single Enter runs them in order. A program that
  did not ask for bracketed paste gets the text as typed, so each newline is an
  Enter.
- On a read-only host the box is drawn disabled and says why. An offline host
  has no box.
- A non-Claude pane needs a `muster-mirror` that announces it can paste into any
  pane. With an older one, the box works only for Claude Code panes.

**Agent sends:**

- A message reaches only a running agent. Otherwise the box is disabled and says
  why.
- A failed turn is stored in the conversation, where the reply would have been.

## Queue: send when Claude Code is idle

Beside **Send**, a mirrored Claude Code window's **Rendered view** has
**Queue**. A queued message is kept by the server (`send_queue`, migration
`0019`) and sent there, so it is sent with the page closed, and every device of
yours shows the same queue under the box, with a **Cancel** for each message not
yet being sent. It is pasted into the window's Claude Code pane, the pane whose
idle it waited on. The Terminal view, dispatched agents and the chief panel have
no Queue.

- **When.** A message is sent once the window's Claude Code session has been
  idle for `MUSTER_SEND_QUEUE_IDLE` (default `30s`), through the same path as
  Send.
- **What idle means.** Claude Code's own session status and the time it wrote
  it (`status` and `statusUpdatedAt` in its sessions file), which
  `muster-mirror` reports per window. "Idle for 30 s" is measured on
  muster's own clock, from when muster first saw that status. `busy` and
  `waiting` (a prompt or a question for you) both hold the queue.
- **One at a time, after every paste.** Whenever anything is pasted into the
  window (a queued message, or your own Send), muster records the status time
  the window held then. The next queued message waits until Claude Code reports
  a status time newer than that, which is the turn the paste started, ended.
  That holds however short the turn was, even one that started and finished
  between two of the host's 5-second refreshes. There is no time-based
  fallback. A window that never reports a newer status keeps its queue held. A
  paste into another pane of a Claude Code window (from the Terminal view)
  holds the queue the same way, until Claude Code's next turn. A Send you make
  in the moment between the server deciding to send a queued message and
  pasting it wins: the queued paste is refused before it reaches the window,
  and the message goes back to the queue behind your Send's turn.
  A Send to a session with no window named counts for the window it went to.
- **Clock skew does not matter.** Status times are the host's clock and are
  compared only with each other, and the idle time is measured on muster's
  clock. A host whose clock is ahead or behind neither stalls the queue nor
  releases a message early.
- 🔴 **Unknown is not idle.** Nothing queued is sent to a window muster cannot
  read. For a `muster-mirror` that does not report the status (an older one) or
  a window whose pane is not Claude Code, Queue is drawn disabled saying why and
  a queue post is refused saying why. A message already queued for such a
  window is held with the reason shown. So is one for a host that reports the
  status without its time.
- **At most 20** messages can wait per window. Finished ones are shown for a
  while and deleted after 7 days.
- **Never twice.** A message is claimed (`queued` → `sending`) before it is sent
  and never put back once anything may have reached the window. (The one way
  back to `queued` is the Send-wins case above, where nothing was pasted.) The
  claim is a 30-second lease that the sending muster renews while it sends. If
  that muster stops in the middle of a send, the message is marked failed ("may
  or may not have been delivered") once the lease has run out, and is not sent
  again. During a rolling update two muster pods overlap, and neither fails or
  finishes the other's send. The lease is kept on the database's clock, so pods
  whose clocks disagree still judge it the same way.

A plugin message your chief [routes to the window](plugins.md#routing-a-message-to-a-session) is queued the same way and waits on the same rules; it carries the route's own time instead of a sign-in (see there).

🔴 **Step-up for a queued message is decided when you QUEUE it.** Queueing
needs the same fresh sign-in a Send needs (the dialog, or the plain confirm page
at `POST /send-queue/confirm`), and the server records the sign-in time of the
session that queued it. When the message is sent, exactly that time is presented
to the same gate, the plugin's own check included. So a queued message is sent
only while the sign-in that queued it is still inside the step-up window. After
that it fails, saying so, and is not sent. The queue never refreshes or extends
the time.

🔴 **A queued message does not outlive the account that queued it.** When a
message is sent, the server checks the account again, from the database: if it
is disabled, or the sign-in that queued the message is at or before the
account's session cutoff (moved by a disable or a password reset) or its latest
**Sign out everywhere** (recorded on the account), the message fails, saying
why, and nothing is pasted. That check is what holds on every server, after a
restart, and for a message whose queue request was still in flight. Disabling
the account (on the admin page or with `muster-server user disable`), or Sign
out everywhere, also cancels every message it has queued ("Cancelled: …").
Signing out of just the one session that queued it does not withdraw it;
Cancel the message instead.

## It works without JavaScript, and a message is delivered once

Every message box is a real `<form method="post">`.

- **With script**, it sends in place.
- **When script cannot help**, the browser submits the form itself. That
  happens if the script's request gets no answer while the browser is online
  (for example a sign-in redirect at an auth proxy, or a proxy error), or if
  script cannot run at all. muster answers with a redirect back to the same
  view, plus a line saying what happened: "Sent.", or the reason it was refused.
- **When the form path finds the sign-in stale**, it shows a plain confirm page
  that asks for the password (`POST /mirror/confirm-send`). That page carries the
  message and delivers it once you confirm.

Each message carries a one-time **nonce**. If a message is posted twice because
the first answer was lost, it is delivered once. Delivered nonces are remembered
in memory for 15 minutes, up to 4096 of them, keyed by owner, nonce and text. A
server restart forgets them.

Pages that carry such a form are served with `Referrer-Policy: same-origin` and
`Cache-Control: no-store`. The rest of the app uses `no-referrer`. Under that
policy, a browser sends `Origin: null` with a form it submits itself, and the
same-origin check refuses `Origin: null`.

## Step-up: "Confirm it's you"

Typing into a mirrored session needs a **fresh sign-in**: one within the step-up
window. This covers both an interactive terminal attach and every mirrored send.
A stale request is refused with 403 before the socket upgrades or anything is
pasted, and the mirror plugin checks again on its own.

Dispatched agents' terminals and sends are **not** step-up gated. Watching
read-only never is.

- **The window** is `MUSTER_STEPUP_WINDOW`, a Go duration. The default is `12h`.
  The server refuses to start with a value outside `1m`–`24h`.
- **What "signed in" means.**
  - In gotrue mode it is the time of the sign-in itself: the access token's `amr`
    timestamp. A token refresh does not move it, and a token with no `amr` is
    never fresh.
  - In password mode it is the session cookie's issue time.
- 🔴 **Freshness belongs to one browser session, never to the user.** It is read
  from the session each request presents, and nothing is recorded per user.
  Confirming on one device leaves your other devices as stale as they were.

When a send or attach is refused as stale, the page shows "Confirm it's you" and
then completes what was waiting: the attach, or sending the **same** text. Nothing
you typed is lost while the dialog is open. You can confirm in two ways:

- **With a passkey.** This is the default when the account has a passkey and the
  browser supports WebAuthn (gotrue mode only). The assertion must include user
  verification and must belong to the *signed-in* user, or it is refused. The
  passkey challenge is requested when the dialog opens, never on page load, and
  not at all for an account with no passkey. Routes:
  `POST /auth/confirm/passkey/options`, then `POST /auth/confirm/passkey/verify`.
- **With the password again** (`POST /auth/confirm`). In password mode this is
  `MUSTER_UI_PASSWORD`.

Both are same-origin only and throttled like sign-in.

The Interactive choice is remembered per host for the browser session. Adding a
passkey has its own fixed window of five minutes, separate from the step-up
window, because a new credential outlives the session that creates it.

![The step-up dialog offering a passkey](img/stepup-passkey.png)
