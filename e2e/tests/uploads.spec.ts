import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { BrowserContext, Page, request as pwRequest } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { binDir, dsn, fakeS3Port, password, repoRoot, uploadsOffPort, uploadsPort, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

// FILE ATTACHMENTS in the Rendered view's composer, in a real browser, against
// a real muster-server (MUSTER_MIRROR=1, password mode) whose MUSTER_S3_* point
// at a FAKE S3 played by this file, and a real muster-mirror over the tunnel
// (the `-tags e2emirror` per-window fake: a paste into the Claude window is
// recorded in <transcript>.pasted AND appended to the transcript as a prompt,
// as Claude Code would). The fake S3 checks what a real one would that this
// spec can: a SigV4 Authorization for the configured access key, and a PUT
// body that hashes to its x-amz-content-sha256. (The signature itself is
// verified against a real MinIO by internal/uploads' TestAgainstARealS3.)
//
// A second server with NO S3 configuration is the feature-off control.
//
// ⚠ WHAT THIS CANNOT SHOW: a real clipboard (the paste is a synthetic
// ClipboardEvent carrying a File), a real drag from the desktop, or an agent
// on another host fetching the link — the link is fetched from this process,
// with no cookies, which is what that host would do.

const origin = `http://localhost:${uploadsPort}`;
const offOrigin = `http://localhost:${uploadsOffPort}`;
const stamp = Date.now().toString(36);
const host = 'e2e-up-' + stamp;
const offHost = 'e2e-upoff-' + stamp;
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'uploads-'));
const tr = path.join(dir, 'abababab-0001.jsonl');
const trOff = path.join(dir, 'cdcdcdcd-0002.jsonl');
const ACCESS = 'e2e-s3-access-not-real';
const SIGNING_KEY = 'e2e-upload-signing-key-not-a-real-one-0123456789';
let server: Running | undefined;
let offServer: Running | undefined;
const procs: ChildProcess[] = [];
let mirrorLog = '';

// A real 2×2 PNG.
const PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAEUlEQVR4nGP4z8AAQv8ZYAwAQ84H+VjtZqAAAAAASUVORK5CYII=',
  'base64',
);

// --- the fake S3 ---------------------------------------------------------------
const objects = new Map<string, Buffer>();
const s3Log: string[] = [];
const s3 = http.createServer((req, res) => {
  const chunks: Buffer[] = [];
  req.on('data', (c) => chunks.push(c));
  req.on('end', () => {
    const body = Buffer.concat(chunks);
    const p = decodeURIComponent((req.url ?? '').split('?')[0]);
    s3Log.push(`${req.method} ${p}`);
    if (!(req.headers.authorization ?? '').startsWith(`AWS4-HMAC-SHA256 Credential=${ACCESS}/`)) {
      res.writeHead(403).end('<Error><Code>AccessDenied</Code></Error>');
      return;
    }
    if (!p.startsWith('/muster-e2e/')) {
      res.writeHead(404).end('<Error><Code>NoSuchBucket</Code></Error>');
      return;
    }
    const key = p.slice('/muster-e2e/'.length);
    if (req.method === 'HEAD' && key === '') return void res.writeHead(200).end();
    if (req.method === 'PUT') {
      const sum = crypto.createHash('sha256').update(body).digest('hex');
      if (sum !== req.headers['x-amz-content-sha256']) return void res.writeHead(400).end('<Error><Code>XAmzContentSHA256Mismatch</Code></Error>');
      objects.set(key, body);
      return void res.writeHead(200).end();
    }
    if (req.method === 'GET') {
      const b = objects.get(key);
      if (!b) return void res.writeHead(404).end('<Error><Code>NoSuchKey</Code></Error>');
      return void res.writeHead(200, { 'Content-Length': String(b.length) }).end(b);
    }
    if (req.method === 'DELETE') {
      objects.delete(key);
      return void res.writeHead(204).end();
    }
    res.writeHead(405).end();
  });
});

const prompt = (text: string) => JSON.stringify({ type: 'user', promptId: 'p-' + text.length, message: { role: 'user', content: text } }) + '\n';
const read = (f: string) => (fs.existsSync(f) ? fs.readFileSync(f, 'utf8') : '');
const viewPath = (h: string) => `/mirror/view?host=${encodeURIComponent(h)}&session=work&window=%401`;
const LINK = /(http:\/\/localhost:\d+\/f\/([A-Za-z0-9_-]{22})\?exp=(\d+)&sig=([A-Za-z0-9_-]{43}))/;

// sign is the server's link signature (internal/uploads.Signer), computed here
// from the key this spec configured: how a test mints an EXPIRED link without
// waiting a minute. Its positive control is a fresh link it signs being served.
const sign = (id: string, exp: number) =>
  crypto.createHmac('sha256', SIGNING_KEY).update(`muster-upload-link-v1\n${id}\n${exp}`).digest('base64url');

function startMirror(h: string, at: string, transcript: string) {
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', h], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  const windows = path.join(dir, h + '.windows');
  fs.writeFileSync(windows, `work|@1|0|1|claude|${Math.floor(Date.now() / 1000) - 60}|${transcript}\n`);
  const p = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: at,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: h,
      MUSTER_MIRROR_INTERACTIVE: '1',
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
      MUSTER_MIRROR_E2E_REFRESH: '250ms',
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  p.stdout?.on('data', (d) => (mirrorLog += d));
  p.stderr?.on('data', (d) => (mirrorLog += d));
  procs.push(p);
}

test.use({ storageState: { cookies: [], origins: [] }, viewport: { width: 1280, height: 800 } });

test.beforeAll(async () => {
  await new Promise<void>((r) => s3.listen(fakeS3Port, '127.0.0.1', () => r()));
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  const base = { MUSTER_MIRROR: '1', MUSTER_AGENT_PROVISIONER: 'noop', MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105' };
  server = await startServer(versionTerm, uploadsPort, {
    ...base,
    MUSTER_PUBLIC_URL: origin,
    MUSTER_S3_ENDPOINT: `http://127.0.0.1:${fakeS3Port}`,
    MUSTER_S3_BUCKET: 'muster-e2e',
    MUSTER_S3_ACCESS_KEY: ACCESS,
    MUSTER_S3_SECRET_KEY: 'e2e-s3-secret-not-real',
    MUSTER_UPLOAD_SIGNING_KEY: SIGNING_KEY,
  });
  // The control: the same build, no S3 configuration at all.
  offServer = await startServer(versionTerm, uploadsOffPort, { ...base, MUSTER_PUBLIC_URL: offOrigin });
  fs.writeFileSync(tr, prompt('the conversation so far'));
  fs.writeFileSync(trOff, prompt('an unconfigured server'));
  startMirror(host, origin, tr);
  startMirror(offHost, offOrigin, trOff);
});

test.afterAll(async () => {
  for (const p of procs) {
    if (p.exitCode === null) {
      const done = new Promise((r) => p.once('exit', r));
      p.kill('SIGTERM');
      await done;
    }
  }
  await server?.stop();
  await offServer?.stop();
  await new Promise((r) => s3.close(r));
  fs.rmSync(dir, { recursive: true, force: true });
});

const signedIn: Record<string, Awaited<ReturnType<BrowserContext['cookies']>>> = {};
async function login(page: Page, at: string) {
  if (signedIn[at]) {
    await page.context().addCookies(signedIn[at]);
    return;
  }
  await page.goto(at + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  signedIn[at] = await page.context().cookies(at);
}

async function openView(page: Page, at = origin, h = host) {
  await login(page, at);
  await page.goto(at + viewPath(h));
  await expect(page.locator('#mv-input'), `the composer is enabled (muster-mirror log:\n${mirrorLog})`).toBeEnabled({ timeout: 20_000 });
}

// fetchNoCookies is what an agent on another host does: a plain GET.
async function fetchNoCookies(url: string) {
  const ctx = await pwRequest.newContext();
  const r = await ctx.get(url, { maxRedirects: 0 });
  const out = { status: r.status(), body: await r.body(), headers: r.headers() };
  await ctx.dispose();
  return out;
}

const chips = (page: Page) => page.locator('#mv-attachments [data-attachment-chip]');

test('an attached image is a thumbnail chip; Send pastes a signed link; the transcript shows the chip; the link downloads, expires and is revoked', async ({ page }) => {
  await openView(page);
  await expect(page.locator('#mv-attach')).toBeVisible();
  const before = read(tr + '.pasted');

  // Attach through the paperclip's file input.
  await page.locator('#mv-file').setInputFiles({ name: 'screen shot.png', mimeType: 'image/png', buffer: PNG });
  await expect(chips(page)).toHaveCount(1);
  const chip = chips(page).first();
  await expect(chip).toHaveAttribute('data-state', 'ready');
  await expect(chip).toContainText('screen_shot.png');
  await expect(chip).toContainText('image/png');
  // The thumbnail is a real image, loaded through the OWNER's route.
  const thumb = chip.locator('img[data-attachment-thumb]');
  await expect(thumb).toHaveAttribute('src', /^\/uploads\/[A-Za-z0-9_-]{22}\/file$/);
  await expect.poll(() => thumb.evaluate((i: HTMLImageElement) => i.complete && i.naturalWidth)).toBe(2);
  // The signed URL is nowhere on the page: not in the box, not in the chip.
  expect(await page.locator('#mv-input').inputValue()).toBe('');
  expect(await page.locator('#mv-compose').innerHTML()).not.toMatch(/sig=|\/f\//);

  await page.locator('#mv-input').fill('look at this');
  await page.locator('#mv-send').click();
  await expect.poll(() => read(tr + '.pasted').length, `the paste arrives (muster-mirror log:\n${mirrorLog})`).toBeGreaterThan(before.length);
  const pasted = read(tr + '.pasted').slice(before.length);
  const m = LINK.exec(pasted);
  expect(m, `a signed link in the pasted text: ${pasted}`).not.toBeNull();
  const [, link, id] = m!;
  expect(pasted).toContain(`look at this\n[attached: screen_shot.png (image/png, ${PNG.length} B) ${link} — download with curl -sSfo screen_shot.png '${link}']`);
  await expect(chips(page), 'the composer is cleared of what was sent').toHaveCount(0);
  await expect(page.locator('#mv-attachments')).toBeHidden();

  // The transcript: the prompt's bubble shows the chip and its thumbnail, and
  // the link appears nowhere in the log.
  const bubble = page.locator('#mv-log [data-bubble="user"]').filter({ hasText: 'look at this' });
  await expect(bubble).toHaveCount(1, { timeout: 15_000 });
  const tchip = bubble.locator('[data-attachment-chip]');
  await expect(tchip).toHaveAttribute('data-upload-id', id);
  await expect(tchip.locator('img')).toHaveAttribute('src', `/uploads/${id}/file`);
  await expect.poll(() => tchip.locator('img').evaluate((i: HTMLImageElement) => i.complete && i.naturalWidth)).toBe(2);
  expect(await page.locator('#mv-log').innerHTML()).not.toMatch(/sig=|\/f\/|curl/);

  // The link, fetched with no session: the bytes, with the server's headers.
  const got = await fetchNoCookies(link);
  expect(got.status).toBe(200);
  expect(Buffer.compare(got.body, PNG)).toBe(0);
  expect(got.headers['content-type']).toBe('image/png');
  expect(got.headers['x-content-type-options']).toBe('nosniff');
  expect(got.headers['cache-control']).toBe('private, no-store');

  // Tampered: 404.
  const u = new URL(link);
  const sig = u.searchParams.get('sig')!;
  u.searchParams.set('sig', (sig[0] === 'A' ? 'B' : 'A') + sig.slice(1));
  expect((await fetchNoCookies(u.toString())).status).toBe(404);
  // Expired: a link this spec signs for a past instant is 404; the control —
  // one it signs for the future — is served, so the 404 is the expiry.
  const past = Math.floor(Date.now() / 1000) - 5;
  const future = Math.floor(Date.now() / 1000) + 600;
  expect((await fetchNoCookies(`${origin}/f/${id}?exp=${past}&sig=${sign(id, past)}`)).status).toBe(404);
  expect((await fetchNoCookies(`${origin}/f/${id}?exp=${future}&sig=${sign(id, future)}`)).status).toBe(200);

  // Revoke, on the transcript's chip: the link is 404 at once, and the object
  // is deleted from the store.
  await tchip.getByRole('button', { name: 'Revoke this link' }).click();
  await expect(tchip.getByRole('button', { name: 'Revoke this link' })).toHaveText('Link revoked');
  expect((await fetchNoCookies(link)).status).toBe(404);
  expect(s3Log).toContain(`DELETE /muster-e2e/uploads/${id}`);
  expect(objects.has(`uploads/${id}`)).toBe(false);
});

test('a pasted image becomes a chip, an attachment alone is a message, and × takes it off and ends its link', async ({ page }) => {
  await openView(page);
  const paste = (name: string) =>
    page.locator('#mv-input').evaluate((el, [b64, n]) => {
      const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
      const dt = new DataTransfer();
      dt.items.add(new File([bytes], n, { type: 'image/png' }));
      el.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
    }, [PNG.toString('base64'), name] as const);

  // × before Send: the upload's link is ended (its object deleted).
  const puts = () => s3Log.filter((l) => l.startsWith('PUT ')).length;
  const putsBefore = puts();
  await paste('removed.png');
  await expect(chips(page)).toHaveCount(1);
  await expect(chips(page).first()).toHaveAttribute('data-state', 'ready');
  expect(puts()).toBe(putsBefore + 1);
  const removedId = await chips(page).first().getAttribute('data-upload-id');
  await chips(page).first().getByRole('button', { name: 'Remove attachment' }).click();
  await expect(chips(page)).toHaveCount(0);
  await expect.poll(() => s3Log.includes(`DELETE /muster-e2e/uploads/${removedId}`)).toBe(true);

  // Pasted, then sent with no text: the message is the attachment line alone.
  const before = read(tr + '.pasted');
  await paste('clip.png');
  await expect(chips(page).first()).toHaveAttribute('data-state', 'ready');
  expect(await page.locator('#mv-input').inputValue(), 'the paste put no text in the box').toBe('');
  await page.locator('#mv-send').click();
  await expect.poll(() => read(tr + '.pasted').length).toBeGreaterThan(before.length);
  const pasted = read(tr + '.pasted').slice(before.length);
  expect(pasted).toMatch(/^%e2e-work-@1 \[attached: clip\.png \(image\/png, \d+ B\) http:\/\/localhost:\d+\/f\/[A-Za-z0-9_-]{22}\?exp=\d+&sig=/);
  expect(pasted).not.toContain(removedId!);
});

test('with no S3 configured there is no paperclip, and the routes do not exist', async ({ page }) => {
  await openView(page, offOrigin, offHost);
  // The same composer, enabled — without the paperclip.
  await expect(page.locator('#mv-send')).toBeVisible();
  await expect(page.locator('#mv-attach')).toHaveCount(0);
  await expect(page.locator('#mv-file')).toHaveCount(0);
  const r = await page.request.post(offOrigin + '/uploads', {
    multipart: { file: { name: 'a.png', mimeType: 'image/png', buffer: PNG } },
    headers: { Origin: offOrigin, 'X-Requested-With': 'XMLHttpRequest' },
  });
  expect(r.status()).toBe(404);
  const id = 'A'.repeat(22);
  const exp = Math.floor(Date.now() / 1000) + 600;
  expect((await fetchNoCookies(`${offOrigin}/f/${id}?exp=${exp}&sig=${sign(id, exp)}`)).status).toBe(404);
  // The control for the 404 above: the ON server answers the same upload 201.
  await login(page, origin);
  const on = await page.request.post(origin + '/uploads', {
    multipart: { file: { name: 'a.png', mimeType: 'image/png', buffer: PNG } },
    headers: { Origin: origin, 'X-Requested-With': 'XMLHttpRequest' },
  });
  expect(on.status(), await on.text()).toBe(201);
});
