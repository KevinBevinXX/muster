// Chromium's VIRTUAL AUTHENTICATOR (the CDP WebAuthn domain), shared by the
// specs that exercise passkeys against the real GoTrue image.
//
// The authenticator is an internal (platform) CTAP2 one with resident keys and
// user verification, auto-confirming presence — a phone's or laptop's built-in
// passkey store, minus the human. It is attached to ONE page; each browser
// context gets its own, so two people's passkeys never meet unless a spec puts
// them on the same page on purpose.
import type { CDPSession, Page } from '@playwright/test';

export interface Authenticator {
  cdp: CDPSession;
  id: string;
}

export async function authenticator(page: Page): Promise<Authenticator> {
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('WebAuthn.enable');
  const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: {
      protocol: 'ctap2', transport: 'internal', hasResidentKey: true,
      hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true,
    },
  });
  return { cdp, id: authenticatorId };
}

// credentialIds is the ids of the (resident) credentials the authenticator holds.
export async function credentialIds(a: Authenticator): Promise<string[]> {
  const { credentials } = await a.cdp.send('WebAuthn.getCredentials', { authenticatorId: a.id });
  return credentials.map((c) => c.credentialId);
}

export async function credentialCount(a: Authenticator): Promise<number> {
  return (await credentialIds(a)).length;
}

// setUserVerified makes the authenticator verify the user (PIN, biometric) or
// merely confirm presence on its next ceremonies.
export async function setUserVerified(a: Authenticator, verified: boolean): Promise<void> {
  await a.cdp.send('WebAuthn.setUserVerified', { authenticatorId: a.id, isUserVerified: verified });
}

export async function removeCredential(a: Authenticator, credentialId: string): Promise<void> {
  await a.cdp.send('WebAuthn.removeCredential', { authenticatorId: a.id, credentialId });
}
