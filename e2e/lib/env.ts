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
// muster-native Web Push: the push spec's own server, with a VAPID keypair
// minted by the binary under test (`muster-server vapid generate`).
export const pushPort = port + 20;
// The seeded `docs-sweeper` agent's own token (e2e/seed). Invented, like every
// value here.
export const agentToken = 'e2e-agent-token-not-a-real-one';
export const baseURL = `http://localhost:${port}`;
export const versionA = 'e2e-a';
export const versionB = 'e2e-b';
export const versionTerm = 'e2e-term';
