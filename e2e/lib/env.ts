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
// AskUserQuestion answered from the Rendered view (mirror-ask.spec.ts): its own
// server (the e2eterm binary, mirror plugin on) and one interactive mirrored
// host, with `muster-mirror ask` run as the hook.
export const mirrorAskPort = port + 80;
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
export const baseURL = `http://localhost:${port}`;
export const versionA = 'e2e-a';
export const versionB = 'e2e-b';
export const versionTerm = 'e2e-term';
