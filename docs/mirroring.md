# Mirrored sessions

A **mirrored session** is a tmux window on one of your own machines, shown as a
card on muster's Agents list. You can watch it, open its terminal, read a Claude
Code conversation as a chat, and send it a message, all from a phone.

Mirroring is off unless the server runs with `MUSTER_MIRROR=1`, and it needs a
database. When it is off, `GET /mirror/connect` and the mirror routes answer
404, and nothing mirrored is drawn.

![A mirrored Claude Code window rendered as a chat](img/mirror-rendered.png)

## How it connects

A host connects to muster; muster never connects to a host. A small binary,
`muster-mirror`, runs on each host. It lists that host's tmux windows and opens
**one** WebSocket to muster, authenticated by a per-host *mirror token*. Terminal
attaches and rendered views are multiplexed back down that one connection.

muster holds no SSH key, address or inbound route to the host, and the host holds
no muster credential except its own token.

```sh
# where the server runs (needs DATABASE_URL); the token is printed once
muster-server mirror token create <host>      # also: mirror token ls, mirror token revoke <id>

# on the host
nix profile install github:ZacxDev/muster#muster-mirror   # or: go install ./cmd/muster-mirror
install -D -m600 /dev/stdin ~/.config/muster-mirror/token  # paste the token
muster-mirror -url https://muster.example.com -token-file ~/.config/muster-mirror/token
```

`<host>` is a lowercase DNS label. `muster-server mirror token create` requires
it. `muster-mirror -host` defaults to the short hostname. A token minted for one
host cannot register another.

Every flag can also be set as a `MUSTER_MIRROR_*` variable (the full list is in
[configuration.md](configuration.md#muster-mirror-host-agent)). Use the variables
when running under a systemd user unit, so the token never appears on a command
line. If muster sits behind an auth proxy, `GET /mirror/connect` must bypass it:
the route checks its own tokens.

## What you see

- **One card per tmux window.** Each window is addressed by its session and
  tmux's own window id (`@12`), not by its index, which tmux renumbers. A card
  says what runs in the window's active pane: Claude Code, opencode, a shell, or
  the command name. A host lists up to 512 windows and reads them with one tmux
  call per refresh, every 5 seconds.
- **Titles.** A card is titled `<session>:<index>`. With `-labels-file` you can
  map a session to a codename: a JSON object `{"<session>": "<codename>"}`. Each
  of that session's windows is then titled `<codename>:<index>`, with the raw
  session name shown beside it. Each entry is checked on its own: an invalid one
  is logged and skipped, and a missing or unreadable file is logged and ignored,
  never fatal.
  When the same title appears on more than one host, every one of them carries
  its host (`mango:1 · <host>`); when it repeats on one host (two sessions given
  one codename), each also carries its tmux address (`mango:1 · <session>:<index>`).
  The card, both view headers and the page title always agree.
- **Most recently active first.** One list across all your hosts, ordered by each
  window's last activity, to the minute. Output in a pane counts as activity, not
  only keystrokes. Ties keep a stable order (host, session, index). An open list
  reorders only when the order actually changes.
- **Not an agent, and not stored.** muster keeps each connected host's window
  list in memory, under the token owner's account. A mirrored window cannot be
  dispatched, deleted, or mistaken for a muster agent. A disconnected host stays
  listed as `offline` for a day. The database holds `mirror_tokens`, as sha256
  hashes, and your pins (below), both under row-level security.
- **Pins.** The pin button on a card (and **Pin to top** in an agent card's ⋯
  menu) puts that entry first in your list, on every device you sign in on.
  Pinned entries keep their own most-recently-active order, ahead of the rest.
  A mirrored pin is the window's address — host, session and tmux window id —
  so it survives renumbering, a list refresh and a muster restart. A pin whose
  window is gone is kept and shown nothing; it comes back with the window. A
  tmux-server restart (a reboot) reissues window ids, so after one a pin names
  whichever window now has that id in a same-named session, or nothing — re-pin.
  The filter still hides a pinned card that does not match.
- **A filter.** One box above the cards narrows dispatched agents and mirrored
  windows together. It is case-insensitive, and every word must match. It shows
  an "N of M" count and runs entirely in the page. A `field:value` term looks
  at one field only, as a substring: `host:<name>` (a dispatched pod agent's
  host is `cluster`), `status:`, `kind:`, `repo:`, `name:`, `account:`,
  `session:`, `ns:`, `mode:`, `state:`, `type:` (`agent` or `mirrored`). Quote a
  value with a space in it: `kind:"claude code"`. A term whose prefix is not one
  of those fields is plain text, so `gold:8` still finds a window called
  `Gold:8`. The filter is kept in the page's `?q=`, so a link reproduces it.

![The Agents list filtered with the search box](img/agents-filter.png)

## Viewing never moves your terminal

A tmux session has one current window, shared by every client attached to it.
If muster attached to your session to show you a window, it would change the
window you are looking at.

So muster never shows a window through your session. Each viewer gets a
short-lived session of its own, `muster-view-<random>`, that **links** the one
window it shows. Its client takes no part in sizing (`ignore-size`), and tmux
removes the session when the viewer leaves (`destroy-unattached`).
`muster-mirror` also kills a viewer session on close, and removes any leftover
ones when it starts. A message is pasted into a *pane id*, which needs no window
switch at all.

⚠ While a viewer is attached, its session is visible to other tmux tooling:
`tmux ls`, a session picker, and `session-created` / `client-attached` hooks.

The terminal never shrinks your window either. The host reports the window's
size, and the browser's terminal uses that size unless the browser's own is
larger. On a phone, a wider window is shown in a scrolling, pinch-zoomable view
with a "Fit width" control.

## Rendered and terminal views

When a window's active pane runs Claude Code, its card opens a **rendered** view
by default. Prompts and replies appear as chat bubbles, and each tool call as one
collapsed row showing the tool, its target, and whether it succeeded, failed or
is still running. It updates live while a turn runs. Thinking is never shown.
Every window has its own view: two Claude Code windows in the same directory each
show their own conversation.

The **Terminal** view is one tap away, for every window. It is an xterm.js
terminal attached through the tunnel.

![A mirrored terminal view with the message box (the screenshot's host is the e2e harness's fake tmux, which prints what reached it)](img/mirror-terminal.png)

Rendered views stream bounded events: at most 300 when opened, taken from the
last 4 MiB of the transcript, with each field capped. The transcript file itself
is never sent, and nothing is stored on the server. Limits per host: 8 open
terminals, 8 rendered views. Limit per server: 64 hosts.

### How a pane's transcript is found

The host walks the pane's process tree to the `claude` process. Then it tries
each of these in order:

1. **The pane map**, if `-pane-map-dir` is set. This is a directory that a
   Claude Code hook keeps current, holding one file per pane: `claude-p<N>.json`
   for pane `%N`. Its first JSON value is:

   ```json
   {"runtime":"claude","pane_id":"%N","tmux_pid":"<tmux server pid>","session_id":"…","transcript_path":"…","claude_pid":"<the claude process>","claude_start":"<its /proc/<pid>/stat start time>"}
   ```

   An entry is used only while it still describes the pane: the same tmux
   server, the same Claude process (pid and start time), and a transcript that
   exists as `<session_id>.jsonl` under the Claude config directory's
   `projects/`. This comes first because Claude Code does not rewrite its
   sessions file when a conversation changes id within one process.
2. **The CLI's own sessions file**, `sessions/<pid>.json`. If Claude Code has
   parked a background job (`/bg`), the host follows it to the job's own
   session.
3. **A guess**: the newest transcript in the process's working directory. This
   is used only when the process has **no** sessions file. A guess that two
   panes would share is dropped.

If the sessions file exists but its transcript cannot be found, the window has
no rendered view; the host does not guess.

The Claude config directory is `$CLAUDE_CONFIG_DIR`, or `~/.claude` when that is
unset. `muster-mirror -list -claude` prints one row per Claude Code window,
ending in how its transcript was resolved: `pane-map`, `sessions` or `guessed`.
`-list -windows` prints every window that would be registered.

## Read-only, interactive, and sending

`muster-mirror` is **read-only by default**. muster then offers no Live keys
switch and refuses an interactive attach. Read-only is enforced in three places:

- muster refuses it;
- the host refuses it, and drops keystrokes on a read-only stream;
- tmux attaches the client read-only (`-f read-only,ignore-size` for a window
  viewer, `attach -r` for a session).

The host opens only windows it listed itself.

🔴 **`-interactive` is a shell on the host.** An interactive tmux client can
switch to any session on that tmux server, or open a new window. So anyone who
can use muster's Live keys switch for the host can run commands there, as the
user `muster-mirror` runs as. The "only listed windows" rule limits what a
read-only viewer can see. It does not limit an attached interactive client.

With `-interactive`, both views get a message box. Typing into one requires a
recent sign-in. See [sending-and-step-up.md](sending-and-step-up.md).

## Scrolling history

The Terminal view's own tmux client draws only the visible screen, so the
browser holds no history. On a host that can, **a vertical drag on the terminal
scrolls the pane's tmux history** (on a desktop, so does the mouse wheel). A
sideways drag still pans a far side wider than the page.

The host does it with tmux **copy-mode** on the window's active pane. It runs
`copy-mode -e` if the pane is not already in copy-mode, then
`send-keys -X -N <n> scroll-up` or `scroll-down`. The scrolled view arrives on
the page like any other output.

- **The pane leaves copy-mode on its own.** `-e` exits when you scroll back to
  the bottom. If nobody scrolls that pane for 30 seconds, the host sends
  `-X cancel`. A disconnect cancels at once. Copy-mode the operator entered
  themselves is scrolled but never cancelled.
- **No step-up is asked for.** While a pane is in copy-mode, tmux sends input to
  copy-mode, not to the program. Scrolling sends nothing but copy-mode's own
  commands, so nothing reaches the program. The route still requires a signed-in
  session, a same-origin request and an `-interactive` host that advertised it.
  Each request moves at most 200 lines, and each user gets 240 requests a second:
  twice what a page sends at full speed (one per frame on a 120 Hz screen).
- **A scroll that does not land is sent again.** On any answer but success, or a
  network failure, the page puts the lines back and retries: on the next frame,
  then after 100, 200 and 400 ms. After five failures in a row it drops what is
  pending and says *Scrolling is not reaching the host*.
- ⚠ **Copy-mode is the pane's, not the viewer's.** Everyone attached to that pane
  sees it scrolled. Someone typing at the pane during copy-mode types into
  copy-mode (its key table), not into the program, until it exits.

A read-only host, or a `muster-mirror` that predates scrolling, advertises
nothing. Its page wires no gesture, and touch pans the terminal as before.

An agent pod's Terminal view has no drag-to-scroll in this release. Only
mirrored windows do.

## Tokens, revocation, rate limits

- muster re-checks each host's token every 30 seconds. A **revoked** token ends
  the host's connection and its open terminals at the next check, and removes the
  host from the list.
- If the database cannot verify the token twice in a row (about a minute), the
  host is disconnected. It stays listed as `offline`, like any other disconnect.
- Repeated bad tokens from one peer (10 a minute) get 429. A valid token is never
  refused by that limit.
- The tunnel is in-process state, so run **one** muster replica.

## Rolling upgrades

muster accepts hosts on tunnel protocol v1 or v2. A v1 host simply has no rendered
view. A v2 `muster-mirror` falls back to v1 against an older muster, so either
side can be upgraded first.

Newer capabilities are announced in the host's hello, without a version bump:
pasting into non-Claude panes, per-window registration, and scrolling a pane's
history. An older host is
never asked for them, and an older muster never asks. A host that registers
windows still sends a per-session list too, so an older muster shows one card per
session as before. An older, per-session host is capped at 64 sessions.
