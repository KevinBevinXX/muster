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
| `plugin` | Your chief drafted a reply to a [plugin](plugins.md) message, and it awaits your approval. The notification's text is muster's own: it never quotes the message or the draft. A tap opens the draft's card on `/plugins`. |
| `attention` | Something new in your [attention queue](attention.md): an agent asked you a question (the notification shows the asker and the question's first line), or a mirrored Claude Code session started waiting at a prompt. One notification per item; it is withdrawn when the item is answered, dismissed, expires or stops waiting. A tap opens the item on `/attention`. A decided reply draft's `plugin` notification is withdrawn the same way. |

Push routes are session-authenticated and same-origin:
`/api/push/{vapid-public-key,subscribe,unsubscribe}` and
`/account/push/{test,preferences,{id}/remove}`.

On iOS, a page must be installed to the Home Screen before it can subscribe. The
page tells you this.

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
