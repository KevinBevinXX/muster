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
import { writeFileAtomic } from '../lib/atomic';

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
// Path-style, one bucket, what minio-go (internal/uploads) sends: HEAD bucket,
// ?lifecycle, PUT (aws-chunked when the payload is streamed), HEAD, GET and
// DELETE object. Multipart uploads are not played: every file here is far
// under one part.
const objects = new Map<string, Buffer>();
const s3Log: string[] = [];
const xmlErr = (res: http.ServerResponse, status: number, code: string) =>
  res.writeHead(status, { 'Content-Type': 'application/xml' }).end(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>${code}</Code><Message>${code}</Message></Error>`);
// unchunk decodes an aws-chunked body: `<hex size>[;chunk-signature=…]\r\n<data>\r\n` … `0…`.
function unchunk(b: Buffer): Buffer {
  const out: Buffer[] = [];
  let at = 0;
  for (;;) {
    const eol = b.indexOf('\r\n', at);
    if (eol < 0) break;
    const size = parseInt(b.subarray(at, eol).toString('latin1').split(';')[0], 16);
    if (!size) break;
    out.push(b.subarray(eol + 2, eol + 2 + size));
    at = eol + 2 + size + 2;
  }
  return Buffer.concat(out);
}
const objHeaders = (b: Buffer) => ({
  'Content-Length': String(b.length),
  ETag: '"' + crypto.createHash('md5').update(b).digest('hex') + '"',
  'Last-Modified': new Date().toUTCString(),
  'Content-Type': 'application/octet-stream',
});
const s3 = http.createServer((req, res) => {
  const chunks: Buffer[] = [];
  req.on('data', (c) => chunks.push(c));
  req.on('end', () => {
    let body = Buffer.concat(chunks);
    const [rawPath, query = ''] = (req.url ?? '').split('?');
    const p = decodeURIComponent(rawPath);
    s3Log.push(`${req.method} ${p}${query ? '?' + query : ''}`);
    if (!(req.headers.authorization ?? '').startsWith(`AWS4-HMAC-SHA256 Credential=${ACCESS}/`)) return xmlErr(res, 403, 'AccessDenied');
    if (!p.startsWith('/muster-e2e')) return xmlErr(res, 404, 'NoSuchBucket');
    const key = p.replace(/^\/muster-e2e\/?/, '');
    if (key === '') {
      if (req.method === 'HEAD') return void res.writeHead(200).end();
      if (query.startsWith('lifecycle')) return xmlErr(res, 404, 'NoSuchLifecycleConfiguration');
      if (query.startsWith('location')) return void res.writeHead(200, { 'Content-Type': 'application/xml' }).end('<LocationConstraint>us-east-1</LocationConstraint>');
      return xmlErr(res, 400, 'NotImplemented');
    }
    if (req.method === 'PUT') {
      const declared = String(req.headers['x-amz-content-sha256'] ?? '');
      if (declared.startsWith('STREAMING')) {
        body = unchunk(body);
        if (String(body.length) !== String(req.headers['x-amz-decoded-content-length'])) return xmlErr(res, 400, 'IncompleteBody');
      } else if (declared !== 'UNSIGNED-PAYLOAD' && declared !== crypto.createHash('sha256').update(body).digest('hex')) {
        return xmlErr(res, 400, 'XAmzContentSHA256Mismatch');
      }
      objects.set(key, body);
      return void res.writeHead(200, { ETag: objHeaders(body).ETag }).end();
    }
    const b = objects.get(key);
    if (req.method === 'HEAD') return b ? void res.writeHead(200, objHeaders(b)).end() : void res.writeHead(404).end();
    if (req.method === 'GET') return b ? void res.writeHead(200, objHeaders(b)).end(b) : xmlErr(res, 404, 'NoSuchKey');
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
  writeFileAtomic(windows, `work|@1|0|1|claude|${Math.floor(Date.now() / 1000) - 60}|${transcript}\n`);
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
  // HEAD: the same headers, from the object's metadata — the store is asked
  // for a HEAD, not the bytes.
  const gets = s3Log.filter((l) => l === `GET /muster-e2e/uploads/${id}`).length;
  const hctx = await pwRequest.newContext();
  const head = await hctx.head(link);
  expect(head.status()).toBe(200);
  expect(head.headers()['content-length']).toBe(String(PNG.length));
  await hctx.dispose();
  expect(s3Log).toContain(`HEAD /muster-e2e/uploads/${id}`);
  expect(s3Log.filter((l) => l === `GET /muster-e2e/uploads/${id}`).length, 'a HEAD fetched no body').toBe(gets);

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

// THE PAPERCLIP IS TRULY SMALL AND INSIDE THE INPUT (operator decision), and
// the session switcher sits below the input row — at a phone and a desktop
// size. 🔴 A REGRESSION TEST: at the base the paperclip is a 44px button BESIDE
// the input, and there is no #session-switcher.
for (const vp of [
  { name: 'phone 390×844', viewport: { width: 390, height: 844 }, mobile: true },
  { name: 'desktop 1280×800', viewport: { width: 1280, height: 800 }, mobile: false },
]) {
  test(`${vp.name}: the paperclip is ~24px inside the input on its left, text clears it as the box grows, and the switcher is below`, async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: vp.viewport, hasTouch: vp.mobile, isMobile: vp.mobile, storageState: { cookies: [], origins: [] } });
    const page = await ctx.newPage();
    try {
      await openView(page);
      const input = page.locator('#mv-input');
      const clip = page.locator('#mv-attach');
      const at = async () => ({ i: (await input.boundingBox())!, c: (await clip.boundingBox())! });
      let { i, c } = await at();
      // ~24px: icon and tap area.
      expect(Math.round(c.width), 'the paperclip is 24px wide').toBe(24);
      expect(Math.round(c.height), 'and 24px tall').toBe(24);
      // Inside the input, on its left.
      expect(c.x, 'inside the input (left edge)').toBeGreaterThanOrEqual(i.x);
      expect(c.x + c.width, 'on the input\'s left side').toBeLessThan(i.x + 40);
      expect(c.y, 'inside the input (top)').toBeGreaterThanOrEqual(i.y);
      expect(c.y + c.height, 'inside the input (bottom)').toBeLessThanOrEqual(i.y + i.height);
      // Text never runs under it: the input's content box starts past its right edge.
      const padLeft = await input.evaluate((el) => parseFloat(getComputedStyle(el).paddingLeft));
      expect(i.x + padLeft, 'the text starts right of the paperclip').toBeGreaterThan(c.x + c.width);
      // The paperclip is the one the file input answers, and still works as a button.
      await expect(clip).toBeEnabled();
      // Typed into, the box grows (to its cap) and the paperclip stays inside it,
      // on the last line; the text still starts past it.
      const h0 = i.height;
      await input.fill(Array.from({ length: 12 }, (_, n) => `line ${n + 1} of a message long enough to wrap on a phone screen`).join('\n'));
      await expect.poll(async () => (await input.boundingBox())!.height, { message: 'the box grows with its text' }).toBeGreaterThan(h0 * 2);
      ({ i, c } = await at());
      expect(i.height, 'up to 40% of the viewport').toBeLessThanOrEqual(Math.ceil(vp.viewport.height * 0.4) + 1);
      expect(c.y + c.height, 'the paperclip is still inside the grown box').toBeLessThanOrEqual(i.y + i.height);
      expect(c.y, 'at its bottom').toBeGreaterThan(i.y + i.height - 40);
      expect(i.x + (await input.evaluate((el) => parseFloat(getComputedStyle(el).paddingLeft))), 'the text still starts right of it').toBeGreaterThan(c.x + c.width);
      // A click on it reaches the file picker (the hidden input's click).
      const chooser = page.waitForEvent('filechooser');
      await clip.click();
      await (await chooser).setFiles({ name: 'tiny.png', mimeType: 'image/png', buffer: PNG });
      await expect(chips(page)).toHaveCount(1);
      await chips(page).first().getByRole('button', { name: 'Remove attachment' }).click();
      await input.fill('');

      // The session switcher: below the input row, inside the pinned composer,
      // on screen, and the page never scrolls sideways.
      const bar = page.locator('#session-switcher');
      await expect(bar).toBeVisible();
      const form = (await page.locator('#mv-form').boundingBox())!;
      const b = (await bar.boundingBox())!;
      expect(b.y, 'the switcher is below the input row').toBeGreaterThanOrEqual(form.y + form.height - 1);
      expect(b.y + b.height, 'and on screen').toBeLessThanOrEqual(vp.viewport.height + 1);
      expect(await page.locator('#mv-compose #session-switcher').count(), 'inside the pinned composer').toBe(1);
      const log = (await page.locator('#mv-log').boundingBox())!;
      const compose = (await page.locator('#mv-compose').boundingBox())!;
      expect(log.y + log.height, 'the conversation ends above the composer').toBeLessThanOrEqual(compose.y + 1);
      const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
      expect(sw, 'no page-level horizontal overflow').toBeLessThanOrEqual(cw);
      // This window is current in All (and not a link).
      await page.locator('#switcher-toggle').click();
      await page.locator('#switcher-toggle').click();
      await expect(page.locator('#switcher-toggle')).toHaveText('All');
      await expect(bar.locator('[data-switcher-list="all"] [data-switcher-current]')).toHaveAttribute('data-switcher-entry', 'mirror');
    } finally {
      await ctx.close();
    }
  });
}

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

// STORED XSS: a file someone uploaded, opened in the signed-in operator's
// browser on muster's origin, must not run script there — not the task
// attachment route, not the owner's view, not the signed link. The detector is
// the payload itself: it logs a marker (any execution at all) and writes
// localStorage (execution IN muster's origin; a sandboxed document's storage
// throws). The positive control serves the same payload as a plain page on
// muster's origin and watches the detector fire, so the zeroes below are
// zeroes of a detector that can see.
test('an uploaded HTML file opened in the browser does not run script in the muster origin', async ({ page }) => {
  await login(page, origin);
  const MARK = 'MUSTER-XSS-RAN-' + stamp;
  const PAYLOAD = Buffer.from(
    `<!DOCTYPE html><html><body><script>console.log(${JSON.stringify(MARK)});` +
      `try{localStorage.setItem('muster-xss',${JSON.stringify(MARK)})}catch(e){};document.title='PWNED'</script></body></html>`,
  );
  const ctx = page.context();
  const stored = async () => {
    const p = await ctx.newPage();
    await p.goto(origin + '/login');
    const v = await p.evaluate(() => localStorage.getItem('muster-xss'));
    await p.evaluate(() => localStorage.removeItem('muster-xss'));
    await p.close();
    return v;
  };
  // open opens url in a fresh tab and reports what the payload did there.
  const open = async (url: string) => {
    const p = await ctx.newPage();
    const logs: string[] = [];
    p.on('console', (m) => logs.push(m.text()));
    const download = p.waitForEvent('download', { timeout: 5_000 }).then(() => true, () => false);
    await p.goto(url).catch(() => undefined); // a download aborts the navigation
    const downloaded = await download;
    const title = await p.title().catch(() => '');
    await p.close();
    return { ran: logs.includes(MARK) || title === 'PWNED', downloaded, inOrigin: await stored() };
  };

  // Positive control: the same bytes served as a page on muster's origin run.
  const probe = origin + '/xss-detector-control-' + stamp;
  await ctx.route(probe, (r) => r.fulfill({ status: 200, contentType: 'text/html', body: PAYLOAD }));
  const control = await open(probe);
  await ctx.unroute(probe);
  expect(control.ran, 'CONTROL: the detector sees the payload run when served as a page').toBe(true);
  expect(control.inOrigin, 'CONTROL: and sees it write muster-origin storage').toBe(MARK);

  // 1. A task attachment, its part claiming text/html.
  const created = await page.request.post(origin + '/tasks', {
    multipart: { body: 'xss probe ' + stamp, attachments: { name: 'notes.html', mimeType: 'text/html', buffer: PAYLOAD } },
    headers: { Origin: origin, 'X-Requested-With': 'XMLHttpRequest', 'HX-Request': 'true' },
  });
  expect(created.status(), await created.text()).toBe(200);
  // The create answers the refreshed task list; the task's own page links
  // its attachments.
  const taskID = /\/tasks\/(\d+)/.exec(await created.text())?.[1];
  expect(taskID, 'the created task is in the refreshed list').toBeTruthy();
  let attachment: string | undefined;
  for (const view of ['/tasks/' + taskID, '/ui/tasks/' + taskID + '/card']) {
    attachment ??= /\/tasks\/\d+\/attachments\/\d+/.exec(await (await page.request.get(origin + view)).text())?.[0];
  }
  expect(attachment, 'the created task links its attachment').toBeTruthy();

  // 2. An upload, through the composer's route: its owner's view and its link.
  const up = await page.request.post(origin + '/uploads', {
    multipart: { file: { name: 'notes.html', mimeType: 'text/html', buffer: PAYLOAD }, host, session: 'work', window: '@1' },
    headers: { Origin: origin, 'X-Requested-With': 'XMLHttpRequest' },
  });
  expect(up.status(), await up.text()).toBe(201);
  const rep = await up.json();
  const link = LINK.exec(rep.line)?.[1];
  expect(link, `a signed link in ${rep.line}`).toBeTruthy();

  for (const [name, url] of [
    ['task attachment', origin + attachment],
    ["upload owner's view", origin + rep.preview],
    ['upload signed link', link!],
  ] as const) {
    const got = await open(url);
    expect(got.ran, `${name}: the uploaded HTML ran script`).toBe(false);
    expect(got.inOrigin, `${name}: the uploaded HTML wrote muster-origin storage`).toBeNull();
    expect(got.downloaded, `${name}: the browser downloads it rather than rendering it`).toBe(true);
  }
});
