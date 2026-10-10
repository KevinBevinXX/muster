// Building and running real muster-server binaries for the browser suite.
//
// 🔴 THE BINARY IS BUILT WITH A VERSION, AND THAT IS THE POINT. The update
// spec needs two servers whose /sw.js bytes differ, and the only honest way to
// get that is two builds with different BuildVersion values: Playwright cannot
// route() a service worker's update fetch, so faking the second script is not
// an option.
import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { binDir, dsn, hookToken, password, repoRoot } from './env';

export function serverBinary(version: string): string {
  return path.join(binDir, `muster-server-${version}`);
}

// tags are Go build tags. Only the terminal spec's binary has one (e2eterm),
// and it is a separate binary so every other spec runs the shipped build.
export function buildServer(version: string, tags: string[] = []): string {
  fs.mkdirSync(binDir, { recursive: true });
  const out = serverBinary(version);
  const tagArgs = tags.length ? ['-tags', tags.join(',')] : [];
  execFileSync('go', ['build', ...tagArgs, '-ldflags', `-X github.com/ZacxDev/muster/internal/api.BuildVersion=${version}`, '-o', out, './cmd/muster-server'], {
    cwd: repoRoot,
    stdio: 'inherit',
  });
  return out;
}

export function goRun(pkg: string, env: Record<string, string>): void {
  execFileSync('go', ['run', pkg], { cwd: repoRoot, stdio: 'inherit', env: { ...process.env, ...env } });
}

export interface Running {
  url: string;
  proc: ChildProcess;
  stop(): Promise<void>;
}

export async function startServer(version: string, port: number, extraEnv: Record<string, string> = {}): Promise<Running> {
  const proc = spawn(serverBinary(version), [], {
    env: {
      ...process.env,
      DATABASE_URL: dsn,
      MUSTER_PORT: String(port),
      MUSTER_STANDALONE: '1',
      MUSTER_UI_PASSWORD: password,
      MUSTER_HOOK_TOKEN: hookToken,
      ...extraEnv,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let log = '';
  proc.stdout?.on('data', (d) => (log += d));
  proc.stderr?.on('data', (d) => (log += d));
  const url = `http://localhost:${port}`;
  const deadline = Date.now() + 20_000;
  for (;;) {
    if (proc.exitCode !== null) throw new Error(`muster-server ${version} exited ${proc.exitCode}:\n${log}`);
    try {
      const r = await fetch(url + '/health');
      if (r.ok) {
        const body = (await r.json()) as { version?: string };
        // The health check names the build, so a stale process on the port
        // (a previous run's) is caught here rather than tested by mistake.
        if (body.version !== version) throw new Error(`port ${port} answers version ${body.version}, want ${version}`);
        break;
      }
    } catch (e) {
      if (e instanceof Error && e.message.startsWith('port ')) throw e;
    }
    if (Date.now() > deadline) throw new Error(`muster-server ${version} did not come up on ${port}:\n${log}`);
    await new Promise((r) => setTimeout(r, 150));
  }
  return {
    url,
    proc,
    async stop() {
      if (proc.exitCode !== null) return;
      const done = new Promise((r) => proc.once('exit', r));
      proc.kill('SIGTERM');
      await done;
    },
  };
}
