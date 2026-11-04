// D53: run the v1 reviewer on PRs v0 already reviewed, each at the head v0 first reviewed, posting nothing.
// Usage: node tools/bench/run.ts <out-dir> <deployment.json> <agent.json> <owner/repo> <pr>...
import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { mkdirSync, writeFileSync } from 'node:fs';
import type { AddressInfo } from 'node:net';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { childPort } from '@quack/session';
import { stubService } from './stub-service.ts';

const [outDir, deployment, agent, repo, ...prs] = process.argv.slice(2);
if (!outDir || !deployment || !agent || !repo || !prs.length) throw new Error('usage: run.ts <out-dir> <deployment.json> <agent.json> <owner/repo> <pr>...');
const main = fileURLToPath(new URL('../../packages/runtime/src/main.ts', import.meta.url));

function v0First(n: string) {
  const reviews = JSON.parse(execFileSync('gh', ['api', `/repos/${repo}/pulls/${n}/reviews`], { encoding: 'utf8', maxBuffer: 64 << 20 })) as any[];
  const first = reviews.find((r) => r.user.login.startsWith('quack-jason'));
  if (!first) throw new Error(`#${n} has no v0 review`);
  return { state: first.state as string, commit: first.commit_id as string, at: first.submitted_at as string, body: first.body as string };
}

// A runtime's graceful close waits for its in-flight run, so SIGKILL follows; a survivor would answer the next run's requestId from its old session.
async function stop(child: ChildProcess) {
  // A child that already exited (a crash) has emitted its exit event; waiting on it then would never return.
  const exited = new Promise<void>((r) => (child.exitCode !== null || child.signalCode !== null ? r() : child.once('exit', () => r())));
  child.kill('SIGTERM');
  const timer = setTimeout(() => child.kill('SIGKILL'), 10_000);
  await exited;
  clearTimeout(timer);
}

// Polls with the same request id instead of one long wait: dedupe returns the run's status, and fetch drops a request
// whose headers take over five minutes.
async function untilSettled(url: string, n: string, deadline: number): Promise<unknown> {
  const body = JSON.stringify({ input: `Review pull request ${repo}#${n}.`, requestId: `bench-${n}` });
  for (;;) {
    const r = (await (await fetch(`${url}/invocations`, { method: 'POST', body })).json()) as { status: string };
    if (r.status === 'done' || r.status === 'unanswered') return r;
    if (Date.now() > deadline) return { ...r, status: 'timeout' };
    await new Promise((res) => setTimeout(res, 5_000));
  }
}

const pins: Record<string, { head: string; at: string }> = {};
const v0 = Object.fromEntries(prs.map((n) => [n, v0First(n)]));
for (const n of prs) pins[`${repo}#${n}`] = { head: v0[n].commit, at: v0[n].at };
mkdirSync(outDir, { recursive: true });
const posted = resolve(outDir, 'posted.jsonl');
const service = stubService(pins, posted).listen(0);
await new Promise((r) => service.once('listening', r));
const serviceUrl = `http://127.0.0.1:${(service.address() as AddressInfo).port}`;

for (const n of prs) {
  const dir = resolve(outDir, `pr-${n}`);
  mkdirSync(join(dir, 'session'), { recursive: true });
  const env = { ...process.env, QUACK_CONFIG: resolve(deployment), QUACK_AGENT: resolve(agent), QUACK_SESSION_DIR: join(dir, 'session'), PORT: '0', QUACK_GH_SERVICE_URL: serviceUrl, QUACK_GH_SERVICE_TOKEN: 'bench' };
  const child = spawn(process.execPath, [main], { env, stdio: ['ignore', 'pipe', 'pipe', 'ipc'] });
  const log: string[] = [];
  child.stdout.on('data', (d) => log.push(String(d)));
  child.stderr.on('data', (d) => log.push(String(d)));
  const started = Date.now();
  let result: unknown;
  try {
    // The runtime clones the repository before it listens, so the port wait shares the run's deadline (as eval's replay does).
    const deadline = started + 30 * 60_000;
    const port = await childPort(child, 'runtime', deadline - Date.now());
    result = await untilSettled(`http://127.0.0.1:${port}`, n, deadline);
  } catch (err) {
    result = { status: 'error', error: String(err) };
  } finally {
    await stop(child);
  }
  const seconds = Math.round((Date.now() - started) / 1000);
  writeFileSync(join(dir, 'result.json'), JSON.stringify({ pr: Number(n), seconds, result, v0: v0[n] }, null, 2));
  writeFileSync(join(dir, 'runtime.log'), log.join(''));
  console.log(`#${n} ${seconds}s ${JSON.stringify(result).slice(0, 160)}`);
}
service.close();
