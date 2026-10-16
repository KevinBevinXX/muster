// A muster-server with the mirror plugin on and a FAKE mirrored host attached:
// the real muster-mirror binary built with `-tags e2emirror`, whose tmux is
// played by files this suite writes (cmd/muster-mirror's e2e fakes). Everything
// between the host and the page — the tunnel, the hub, the sockets — is real.
//
// ONE copy: every spec that runs a fake host goes through here (pins, mirrored,
// mirror-chat, and the specs written with it). Three private copies of it
// had grown before it existed.
import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { Page } from '@playwright/test';
import { expect } from './fixtures';
import { binDir, dsn, password, repoRoot, versionTerm } from './env';
import { serverBinary, startServer, Running } from './server';

const mirrorBin = path.join(binDir, 'muster-mirror-e2e');

// A Claude Code transcript record, as the CLI writes it.
const rec = (o: object) => JSON.stringify(o) + '\n';
export const cc = {
  prompt: (t: string) => rec({ type: 'user', promptId: 'p', origin: { kind: 'human' }, message: { role: 'user', content: t } }),
  reply: (t: string) => rec({ type: 'assistant', message: { content: [{ type: 'text', text: t }] } }),
  toolUse: (id: string, name: string, input: object) => rec({ type: 'assistant', message: { content: [{ type: 'tool_use', id, name, input }] } }),
  toolResult: (id: string, out: string, isError = false) =>
    rec({ type: 'user', promptId: 'p', message: { content: [{ type: 'tool_result', tool_use_id: id, content: out, is_error: isError }] } }),
  taskNotice: (status: string, summary: string) =>
    rec({
      type: 'user',
      promptId: 'p',
      origin: { kind: 'task-notification' },
      message: { role: 'user', content: `<task-notification>\n<task-id>t1</task-id>\n<status>${status}</status>\n<summary>${summary}</summary>\n</task-notification>` },
    }),
};

export class MirrorRig {
  readonly origin: string;
  readonly host: string;
  readonly dir = fs.mkdtempSync(path.join(os.tmpdir(), 'muster-e2e-rig-'));
  server?: Running;
  proc?: ChildProcess;
  log = '';
  private token = '';

  // decimalHost names the host with a DECIMAL timestamp, so it carries no
  // letter a search spec types; the default is the shorter base-36 one the
  // layout specs were written against.
  constructor(readonly port: number, hostPrefix: string, private readonly serverEnv: Record<string, string> = {}, opts: { decimalHost?: boolean } = {}) {
    this.origin = `http://localhost:${port}`;
    this.host = hostPrefix + '-' + (opts.decimalHost ? String(Date.now()) : Date.now().toString(36));
  }

  // running is whether the fake host process is up.
  running(): boolean {
    return !!this.proc && this.proc.exitCode === null;
  }

  async start(): Promise<void> {
    execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
    this.server = await startServer(versionTerm, this.port, {
      MUSTER_MIRROR: '1',
      MUSTER_AGENT_PROVISIONER: 'noop',
      MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
      MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
      ...this.serverEnv,
    });
    this.token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', this.host], { env: { ...process.env, DATABASE_URL: dsn } })
      .toString()
      .trim();
    expect(this.token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  }

  // transcript writes a Claude Code transcript file and returns its path.
  transcript(body: string, name = 'session'): string {
    const p = path.join(this.dir, `${name}-${Date.now().toString(36)}.jsonl`);
    fs.writeFileSync(p, body);
    return p;
  }

  // host (re)starts the fake host with the given fake-tmux environment
  // (MUSTER_MIRROR_E2E_SESSIONS, _CLAUDE, _WINDOWS_FILE, …).
  async startHost(env: NodeJS.ProcessEnv): Promise<void> {
    await this.stopHost();
    this.log = '';
    this.proc = spawn(mirrorBin, [], {
      env: {
        ...process.env,
        MUSTER_MIRROR_URL: this.origin,
        MUSTER_MIRROR_ALLOW_HTTP: '1',
        MUSTER_MIRROR_TOKEN: this.token,
        MUSTER_MIRROR_HOST: this.host,
        ...env,
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    this.proc.stdout?.on('data', (d) => (this.log += d));
    this.proc.stderr?.on('data', (d) => (this.log += d));
  }

  async stopHost(): Promise<void> {
    if (!this.proc || this.proc.exitCode !== null) return;
    const done = new Promise((r) => this.proc!.once('exit', r));
    this.proc.kill('SIGTERM');
    await done;
  }

  async stop(): Promise<void> {
    await this.stopHost();
    await this.server?.stop();
    fs.rmSync(this.dir, { recursive: true, force: true });
  }

  async login(page: Page): Promise<void> {
    await page.goto(this.origin + '/login');
    await page.fill('#muster-password', password);
    await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  }

  // waitListed waits until the host's sessions are on the Agents list (the
  // tunnel is up and has reported), with the host's log in the failure.
  async waitListed(page: Page, needle: string): Promise<void> {
    await expect
      .poll(async () => (await (await page.request.get(this.origin + '/ui/agents')).text()).includes(needle), {
        message: `the host's sessions are listed (muster-mirror log:\n${this.log})`,
        timeout: 20_000,
      })
      .toBe(true);
  }

  viewPath(session: string): string {
    return `/mirror/view?host=${encodeURIComponent(this.host)}&session=${encodeURIComponent(session)}`;
  }

  terminalPath(session: string): string {
    return `/mirror/terminal?host=${encodeURIComponent(this.host)}&session=${encodeURIComponent(session)}`;
  }
}

// mkchat runs e2e/mkchat: an agent of the caller's own with a persisted chat
// (or, with del, removes it). Every value is invented fixture data.
export function mkchat(name: string, messages: object[] | null, extraEnv: Record<string, string> = {}): void {
  execFileSync('go', ['run', './e2e/mkchat'], {
    cwd: repoRoot,
    env: { ...process.env, DATABASE_URL: dsn, MUSTER_E2E_AGENT: name, ...(messages === null ? { MUSTER_E2E_DELETE: '1' } : {}), ...extraEnv },
    input: messages === null ? '' : JSON.stringify(messages),
  });
}
