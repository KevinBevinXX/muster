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
// The seeded `docs-sweeper` agent's own token (e2e/seed). Invented, like every
// value here.
export const agentToken = 'e2e-agent-token-not-a-real-one';
export const baseURL = `http://localhost:${port}`;
export const versionA = 'e2e-a';
export const versionB = 'e2e-b';
export const versionTerm = 'e2e-term';
