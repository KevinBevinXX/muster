// Outliving a GoTrue access token, for the specs that need muster to REFUSE one
// as expired and take the refresh path (gotrue-login, passkeys, invites).
//
// The compose GoTrue mints 5 s tokens (GOTRUE_JWT_EXP). muster's verifier adds
// a clock skew to exp — gotrue.DefaultSkew, 30 s, in every shipped binary — so
// at the production skew a token is refused only 35 s after it was minted.
// These specs run the `e2eterm` server build, whose verifier takes its skew
// from MUSTER_E2E_GOTRUE_SKEW (cmd/muster-server/e2egotrue.go; the shipped
// binary has no reader of it). With 1 s the refusal comes 6 s after minting.
//
// 🔴 THE WAIT IS COMPUTED FROM THE TOKEN, NOT GUESSED: outliveAccessToken reads
// the token's own exp and returns once now >= exp + skew (the verifier's own
// test, gotrue.Verifier.Verify) plus a margin. Waiting less would test a token
// muster still accepts — not the refresh path at all.

export const gotrueSkewSeconds = 1;
export const gotrueSkewEnv = { MUSTER_E2E_GOTRUE_SKEW: `${gotrueSkewSeconds}s` };

// marginMs covers exp's whole-second rounding and the server's clock read.
const marginMs = 1_000;

function claims(token: string): { exp?: number; iat?: number } {
  const part = token.split('.')[1];
  if (!part) throw new Error(`not a JWT: ${token.slice(0, 16)}…`);
  return JSON.parse(Buffer.from(part, 'base64url').toString('utf8'));
}

export async function outliveAccessToken(token: string): Promise<void> {
  const { exp, iat } = claims(token);
  if (typeof exp !== 'number' || typeof iat !== 'number') throw new Error('the access token has no exp/iat');
  // The compose file's 5 s lifetime; a longer one means the spec would wait
  // minutes for a reason this helper should name rather than time out on.
  if (exp - iat > 10) throw new Error(`the access token lives ${exp - iat} s; the e2e GoTrue should mint 5 s tokens (GOTRUE_JWT_EXP)`);
  const wait = (exp + gotrueSkewSeconds) * 1000 + marginMs - Date.now();
  if (wait > 0) await new Promise((r) => setTimeout(r, wait));
}
