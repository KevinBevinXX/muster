// The e2e suite's fixed inputs, spelled once.
//
// Every value here is invented for the suite: the password and token guard a
// throwaway server on localhost, and the database name must end in _e2e (the
// seed refuses anything else — it truncates what it seeds).
import path from 'node:path';

export const repoRoot = path.resolve(__dirname, '..', '..');
export const e2eRoot = path.resolve(__dirname, '..');
export const binDir = path.join(e2eRoot, '.bin');
export const stateDir = path.join(e2eRoot, '.state');
export const authState = path.join(stateDir, 'auth.json');

export const dsn = process.env.MUSTER_E2E_DATABASE_URL ?? '';
// 🔴 With this set, a missing database FAILS the run instead of skipping it. A
// skip is invisible in a pass/fail verdict; CI sets it, and the verdict step
// also caps the skip count (e2e/verdict.mjs).
export const requireFull = process.env.MUSTER_E2E_REQUIRE_FULL === '1';

export const password = 'e2e-operator-password-not-a-real-one';
export const hookToken = 'e2e-hook-token-not-a-real-one';

// The shared server every spec but the update spec uses, and the version it
// is built with. The update spec runs its own pair on its own port, because a
// service worker is per ORIGIN and a fresh port is a fresh origin.
export const port = Number(process.env.MUSTER_E2E_PORT ?? 18181);
export const updatePort = port + 1;
// The agent-kinds spec's own server (claude-code enabled over the noop driver).
export const kindsPort = port + 2;
// The terminal spec's own server: a binary built with `-tags e2eterm`, whose
// noop driver plays a terminal (cmd/muster-server/e2eterm.go).
export const termPort = port + 3;
// The UI-polish spec's own server (kinds enabled over the noop driver).
export const polishPort = port + 4;
// The provisioning-indicator spec's own server: the e2eterm binary with its
// readiness hold armed (MUSTER_E2E_HOLD_READY_DIR, cmd/muster-server/e2eterm.go).
export const provPort = port + 5;
// The connector spec's own server (the e2eterm binary, whose connector proxy may
// reach a FAKE upstream on loopback), its dedicated connect listener, and the
// fake upstream itself.
export const connectorsPort = port + 6;
export const connectPort = port + 7;
export const fakeUpstreamPort = port + 8;
export const mirrorPort = port + 9;
// accounts PR 4: a gotrue-mode server (machine port + browser listener) and
// the real GoTrue it signs in against (e2e/gotrue/compose.yml).
export const gotrueMainPort = port + 10;
export const gotrueBrowserPort = port + 11;
export const gotruePort = Number(process.env.MUSTER_E2E_GOTRUE_PORT ?? port + 12);
export const gotrueSecret = 'e2e-gotrue-jwt-secret-not-a-real-one-0123456789';
// accounts PR 5: the invites spec's own gotrue-mode server and real GoTrue, so
// it never shares a GoTrue database (or a claimed admin) with gotrue-login.
export const invitesMainPort = port + 13;
export const invitesBrowserPort = port + 14;
export const invitesGotruePort = Number(process.env.MUSTER_E2E_INVITES_GOTRUE_PORT ?? port + 15);
// The terminal-overflow spec's own e2eterm server (its fake pod reports a
// session window: MUSTER_E2E_TERM_WINDOW).
export const overflowPort = port + 16;
// The mirror step-up spec: a gotrue-mode server with the mirror plugin on
// (machine port + browser listener) and its own real GoTrue.
export const stepupMainPort = port + 17;
export const stepupBrowserPort = port + 18;
export const stepupGotruePort = Number(process.env.MUSTER_E2E_STEPUP_GOTRUE_PORT ?? port + 19);
// Passkeys: their own gotrue-mode server and real GoTrue (passkeys ON, RP
// localhost, origin = this browser port), for the same reason as invites.
export const passkeysMainPort = port + 20;
export const passkeysBrowserPort = port + 21;
export const passkeysGotruePort = Number(process.env.MUSTER_E2E_PASSKEYS_GOTRUE_PORT ?? port + 22);
// muster-native Web Push: the push spec's own server, with a VAPID keypair
// minted by the binary under test (`muster-server vapid generate`).
export const pushPort = port + 23;
// Per-window mirrors: the mirror-windows spec's own server (the e2eterm binary
// with the mirror plugin on), so its host and its cards are no other spec's.
export const windowsPort = port + 24;
// The agent view-switch and composer spec's own e2eterm server (kinds enabled).
export const agentComposerPort = port + 25;
// Dictation: the dictation spec's own server (the e2eterm binary, mirror plugin
// on, kinds enabled, MUSTER_STT_* pointing at a fake speech-to-text service),
// and that fake service.
export const dictationPort = port + 26;
export const fakeSttPort = port + 27;
// Duplicate mirror labels: the spec's own server (the e2eterm binary with the
// mirror plugin on), so only ITS two hosts are listed — the clash it draws is
// between them and no other spec's host.
export const dupLabelsPort = port + 28;
// Plugins: a gotrue-mode server with MUSTER_PLUGINS=1 and the connector store
// (for /mcp, on its own connect port), and its own real GoTrue with passkeys on —
// so no other spec's server ever has plugins on.
export const pluginsMainPort = port + 29;
export const pluginsBrowserPort = port + 30;
export const pluginsGotruePort = Number(process.env.MUSTER_E2E_PLUGINS_GOTRUE_PORT ?? port + 31);
export const pluginsConnectPort = port + 32;
// Pins: the pins spec's own server (the e2eterm binary with the mirror plugin
// on), so its mirrored host and its pins' order are no other spec's.
export const pinsPort = port + 36;
// The mirrored chat spec's own server (the e2eterm binary with the mirror plugin
// on): notices, bubble clamps, the message index and context usage.
export const mirrorChatPort = port + 50;
// Layout shift (cls.spec.ts): the FULL server (every optional store built) and
// its connector listener, and a gotrue-mode server's two listeners for the
// public sign-in pages.
export const clsFullPort = port + 37;
export const clsConnectPort = port + 46;
export const clsGotrueMainPort = port + 43;
export const clsGotrueBrowserPort = port + 44;
// …and the signed-in pages (cls.spec.ts): a gotrue-mode server (passkeys and
// plugins on) with its own real GoTrue and its connector listener.
export const clsSignedMainPort = port + 51;
export const clsSignedBrowserPort = port + 52;
export const clsSignedGotruePort = Number(process.env.MUSTER_E2E_CLS_GOTRUE_PORT ?? port + 53);
export const clsSignedConnectPort = port + 54;
// The composer-autosize, tool-group and agents-search specs' own servers (each
// the e2eterm binary with the mirror plugin on and a fake mirrored host).
export const composerFitPort = port + 38;
export const toolGroupsPort = port + 39;
export const agentsSearchPort = port + 47;
// The pickers spec's own e2eterm server, and its fake upstreams (GitHub,
// OpenRouter's model list, a permission router) on one loopback port.
export const pickersPort = port + 48;
export const pickersFakePort = port + 49;
// The send queue: its own server (the e2eterm binary, mirror plugin on, a short
// MUSTER_SEND_QUEUE_IDLE) and its two mirrored hosts — one that reports Claude
// Code's status and one that plays an older muster-mirror.
export const sendQueuePort = port + 45;
// Session focus (focus.spec.ts): its own server (the e2eterm binary, mirror
// plugin on) and one mirrored host whose Claude window reports a session id.
export const focusPort = port + 55;
// Session routing (session-routing.spec.ts): its own server (plugins, mirror
// plugin and routing on) and its connect listener (the chief's /mcp door).
export const routingPort = port + 56;
export const routingConnectPort = port + 57;
// The Rendered view composer's split Send/Queue button and its `/` and `:`
// menu (composer-menu.spec.ts): its own server (the e2eterm binary, mirror
// plugin on) and two mirrored hosts — one that reports its catalog and one
// that plays an older muster-mirror.
export const composerMenuPort = port + 58;
// The attention queue (attention.spec.ts): its own server (the e2eterm binary,
// mirror plugin on) with its connect listener (the asking agent's /mcp door) and
// one mirrored host whose Claude window is waiting on the person.
export const attentionPort = port + 62;
export const attentionConnectPort = port + 63;
// File attachments (uploads.spec.ts): the server with S3 configured, the one
// without, and the fake S3. Far from the others so a sibling spec added in the
// meantime is unlikely to land on them.
export const uploadsPort = port + 70;
export const uploadsOffPort = port + 71;
export const fakeS3Port = port + 72;
// The Agents list's host tabs (host-tabs.spec.ts): its own server (the
// e2eterm binary, mirror plugin on) and up to three mirrored hosts. A high
// offset, clear of the specs being added beside it.
export const hostTabsPort = port + 64;
// AskUserQuestion answered from the Rendered view (mirror-ask.spec.ts): its own
// server (the e2eterm binary, mirror plugin on) and one interactive mirrored
// host, with `muster-mirror ask` run as the hook.
export const mirrorAskPort = port + 80;
// …and the mirrored host's own loopback MCP endpoint (muster-mirror -mcp-listen).
export const attentionMirrorMCPPort = port + 65;
// The fake service's bearer token. Invented, like every value here.
export const sttToken = 'e2e-stt-token-not-a-real-one';
// The docs screenshots (e2e/docshots, `make screenshots`, never in the suite):
// a gotrue-mode server with the mirror plugin on, and its own real GoTrue.
export const docsMainPort = port + 40;
export const docsBrowserPort = port + 41;
export const docsGotruePort = Number(process.env.MUSTER_E2E_DOCS_GOTRUE_PORT ?? port + 42);
// The seeded `docs-sweeper` agent's own token (e2e/seed). Invented, like every
// value here.
export const agentToken = 'e2e-agent-token-not-a-real-one';
// The chief's sessions on more than one runtime (chief-runtimes.spec.ts): its
// own server (the e2eterm binary, both kinds over the noop driver, both session
// runtimes offered) and the fake OpenRouter model list it searches.
export const chiefRuntimesPort = port + 90;
export const chiefRuntimesFakePort = port + 91;
// An answer delivered to the chief (attention-delivery.spec.ts): its own server
// (the e2eterm binary, the agent gateway on, every agent endpoint pointed at the
// spec's fake runtime), its /mcp door, and that fake runtime.
export const attentionDeliveryPort = port + 96;
export const attentionDeliveryConnectPort = port + 97;
export const attentionDeliveryFakePort = port + 98;
// The Agents list's cards (agent-cards.spec.ts): its own server (the e2eterm
// binary, mirror plugin on), its connect listener (the asking agent's /mcp
// door), and one mirrored host with a Claude window and two other windows.
export const cardsPort = port + 100;
export const cardsConnectPort = port + 101;
// Host icons and the Agents list's scroll positions (host-icons.spec.ts): its
// own server (the e2eterm binary, mirror plugin on) and two mirrored hosts.
export const hostIconsPort = port + 110;
// A Hermes window's read-only rendered view and its card (hermes-view.spec.ts):
// its own server (the e2eterm binary, mirror plugin on) and one mirrored host
// whose Hermes conversation is a real state.db the fake host writes.
export const hermesViewPort = port + 120;
// The Agents list UX batch (agents-list-ux.spec.ts): its own server (the
// e2eterm binary, mirror plugin on) and one mirrored host with four windows.
export const listUxPort = port + 137;
// Live work counts and identity colours on mirrored cards
// (agent-cards-work.spec.ts): its own server (mirror plugin on), one mirrored
// host with four windows and an OLDER host that sends neither message.
export const workCardsPort = port + 145;
// A mirrored window's Rendered | Terminal switch keeping both views' host
// streams (view-swap.spec.ts): its own server (mirror plugin on) and one
// per-window mirrored host with two Claude Code windows.
export const viewSwapPort = port + 165;
// The long Agents list's layout shift (cls.spec.ts "a long Agents list"): its
// own server (the e2eterm binary, mirror plugin on) and one mirrored host with
// sixty-odd windows. Clear of the offsets above and of the specs beside them.
export const clsListPort = port + 170;
// The Agents list's Idle section (agents-idle.spec.ts): its own server (the
// e2eterm binary, mirror plugin on) and one mirrored host.
export const agentsIdlePort = port + 175;
// Host tab order, named-host icons and the host chip (host-names.spec.ts): its
// own server (the e2eterm binary, mirror plugin on) and four mirrored hosts.
export const hostNamesPort = port + 180;
// The side nav on every signed-in page (sidenav.spec.ts): its own server (the
// e2eterm binary, mirror and plugins on) and one per-window mirrored host.
export const sidenavPort = port + 185;
// The rendered view's snapshot (mirror-snapshot.spec.ts): its own server (the
// e2eterm binary, mirror plugin on) and one per-window mirrored host.
export const mirrorSnapPort = port + 190;
// The back/forward cache control (bfcache.spec.ts): a two-page static site the
// spec serves itself, so whether a page is restored depends on the browser and
// the harness alone, never on muster's headers.
export const bfcacheControlPort = port + 194;
// The long-transcript layout shift on a mirrored view (cls-long-transcript.spec.ts):
// its own server (the e2eterm binary, mirror plugin on) and one mirrored host.
export const clsLongMirrorPort = port + 196;
// Suggested replies' chip row (suggested-replies.spec.ts): its own server
// (the e2eterm binary, mirror plugin on, the model pointed at the spec's fake)
// and that fake chat-completions endpoint.
export const suggestPort = port + 197;
export const suggestFakePort = port + 198;
// Back and Forward restoring the view pages (bfcache-restore.spec.ts): its own
// mirrored server (the e2eterm binary, mirror plugin on) and one mirrored host,
// and a port of its own for the build A / build B pair (a deploy while a page
// sits in the back/forward cache).
export const bfcacheRestorePort = port + 150;
export const bfcacheBuildPort = port + 151;
// …and a static page of its own on another origin, to leave a page for when
// the muster page left behind must not answer anything meanwhile.
export const bfcacheAwayPort = port + 152;
export const baseURL = `http://localhost:${port}`;
export const versionA = 'e2e-a';
export const versionB = 'e2e-b';
// A third build: the update spec's "a newer build after a dismissal" case.
export const versionC = 'e2e-c';
export const versionTerm = 'e2e-term';
