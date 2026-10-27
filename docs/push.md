# Web push and the installable app

## Web push

muster sends Web Push notifications under its own VAPID identity. To enable them,
set the three variables together. Setting only some of them refuses to boot.

```sh
muster-server vapid generate     # prints MUSTER_VAPID_PUBLIC_KEY=… and MUSTER_VAPID_PRIVATE_KEY=…
export MUSTER_VAPID_SUBJECT='mailto:you@example.com'   # or an https: URL
```

⚠ Generating a new keypair strands every existing subscription, so devices have
to enable notifications again.

When push is off:

- no Enable button is drawn;
- `/api/push/vapid-public-key` answers 503;
- nothing is sent.

**Account → Notifications** (on `/account`) lists your subscribed devices. Each
one has a "Send a test notification" button and a Remove button. Each event type
has its own toggle, and all are on by default:

| event | when |
|---|---|
| `review` | A task becomes ready for review. When it leaves review, the notification is withdrawn. |
| `connector` | An agent requests access to a connector. |
| `agent` | A task's agent comes up. |
| `created` | A machine (an agent, the CLI) created tasks. |
| `comment` | A machine commented on a task. |
| `plugin` | The target agent drafted a reply to a [plugin](plugins.md) message, and it awaits your approval. The notification's text is muster's own: it never quotes the message or the draft. A tap opens the draft's card on the Asks page's Reply drafts tab (`/attention?tab=drafts#draft-<id>`). |
| `attention` | Something new in your [attention queue](attention.md): an agent asked you a question (the notification shows the asker and the question's first line), or a mirrored Claude Code session started waiting at a prompt. One notification per item; it is withdrawn when the item is answered, dismissed, expires or stops waiting. A tap opens the item's session when it has one (a waiting window's or a held question's rendered view, an asking agent's page, an asking mirrored session's window) and its item on `/attention` otherwise, decided when tapped (`/attention/open/<key>`; [attention.md](attention.md)). A decided reply draft's `plugin` notification is withdrawn the same way. |

Push routes are session-authenticated and same-origin:
`/api/push/{vapid-public-key,subscribe,unsubscribe}` and
`/account/push/{test,preferences,{id}/remove}`.

On iOS, a page must be installed to the Home Screen before it can subscribe. The
page tells you this.

### Which push services

A subscription's endpoint is a URL that muster later POSTs to, so muster accepts
only endpoints on a known push service, on `https` and the default port:

- the browser push services: FCM, Mozilla autopush, Apple, and Windows (WNS).
  These are the only hosts a **browser** subscription (Enable, in the header)
  may name.
- your own hosts, listed in `MUSTER_PUSH_EXTRA_HOSTS`, for **paired phones
  only**: a self-hosted ntfy that delivers UnifiedPush to the
  [Android app](design/android.md). Each matches by its exact name only, with no
  subdomains or wildcards. It is empty by default. A browser can never subscribe
  on one of these hosts.

**The list is checked again at every send.** A device whose host is no longer
allowed, for example because you took it out of `MUSTER_PUSH_EXTRA_HOSTS`, is
sent nothing and removed, and the send is counted as
`muster_push_delivery_total{result="disallowed"}`.

**muster never follows a redirect on a push send.** A push service that answers
with a 3xx counts as a failed send, and the device's failure count goes up. This
applies to every push service. Without it, a listed host could redirect a send to
a host that is not on the list.

muster does not check which address a listed host resolves to. An in-cluster
ntfy that resolves to a private address works.

⚠ If your ntfy host is behind a CDN, its bot filter may refuse requests with an
unusual `User-Agent`. muster sends Go's default one (`Go-http-client/1.1`).
A phone's registration fails with an error when muster's test push to it is
refused, so a CDN rule that blocks muster shows up there.

### Paired phones (payload v2)

A phone paired with the [Android app](design/android.md) registers its
UnifiedPush endpoint through the device routes ([auth.md](auth.md#paired-devices)).
It receives the same events as a browser, under the same toggles and rate cap,
as **payload v2**:

```json
{"v": 2, "type": "review", "id": "42", "tag": "task-42",
 "title": "<WHO>", "body": "<WHERE>", "url": "/tasks/42"}
```

- `title` and `body` are the browser notification's own lines, WHO and WHERE,
  never WHAT. They are cut to 100 and 200 characters, and never in the middle
  of a character a reader sees as one (an accent, an emoji, a flag). The app
  fetches anything more itself.
- **A phone's push is exactly 4096 bytes.** muster pads every encrypted push to
  one 4096-byte record, so a payload v2 may be at most 3993 bytes
  (4096 − 86 header − 16 tag − 1 delimiter); the encryption library itself
  refuses anything larger. So muster never sends ntfy a message over its
  default 4096-byte limit, which ntfy would accept with HTTP 200 and silently
  cut short, leaving the phone unable to decrypt it (that matters for senders
  that do not pad to one record). muster checks the size before sending, so an
  over-budget payload is **refused, not sent**: it is logged
  (`push: REFUSED …`), counted as `muster_push_delivery_total{result="oversize"}`,
  and does not count as the phone failing.
- A withdrawal is `{"v": 2, "type": "resolved", "id": …, "tag": …}` and shows
  nothing. Phones always get it; Safari browsers are still spared it.
- Which payload a device gets depends on the kind recorded with its
  subscription, not on its push host.
- There are no `actions`, `conv` or `state` fields yet. They come with the
  phases that add the routes and the widgets that use them.
- ⚠ **ntfy does not check VAPID** on UnifiedPush (`up*`) topics, so anyone who
  learns a phone's endpoint can publish to it. The app's rule of dropping every
  message it cannot decrypt is the **only** check that a message came from
  muster, and it is load-bearing.
- muster sets no Web Push `Topic` header, and ntfy ignores one anyway, so a
  push that was not delivered is not replaced by a newer one.

## Installable app (PWA)

- **The manifest** (`/manifest.webmanifest`) declares a standalone display, any
  and maskable icons at 192 and 512 px, and two narrow screenshots. Neither the
  manifest nor `/sw.js` needs a session.
- **An Install button** appears when the browser offers installation, and never
  inside an installed app.
- **The service worker** (`/sw.js`) is stamped with the build version, so each
  deploy counts as an update. An open page offers the update instead of taking
  it. The worker serves an offline shell, but only while you are offline.
- **The share target:** sharing a link or text to muster opens a pre-filled
  new-task sheet. Nothing is saved until you submit it.
- **Shortcuts:** New task, Tasks, Agents.
- **The badge:** the app icon shows the ready-for-review count, kept current over
  the page's event stream.
