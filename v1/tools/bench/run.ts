// D53: run the v1 reviewer on PRs v0 already reviewed, each at the head v0 first reviewed, posting nothing.
// Usage: node tools/bench/run.ts <out-dir> <deployment.json> <agent.json> <owner/repo> <pr>...
import { execFileSync, spawn } from 'node:child_process';
import { mkdirSync, writeFileSync } from 'node:fs';
import type { AddressInfo } from 'node:net';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { stubService } from './stub-service.ts';

const [outDir, deployment, agent, repo, ...prs] = process.argv.slice(2);
if (!outDir || !deployment || !agent || !repo || !prs.length) throw new Error('usage: run.ts <out-dir> <deployment.json> <agent.json> <owner/repo> <pr>...');
const main = fileURLToPath(new URL('../../packages/runtime/src/main.ts', import.meta.url));

function v0First(n: string) {
  const reviews = JSON.parse(execFileSync('gh', ['api', `/repos/${repo}/pulls/${n}/reviews`], { encoding: 'utf8', maxBuffer: 64 << 20 })) as any[];
  const first = reviews.find((r) => r.user.login.startsWith('quack-jason'));
  if (!first) throw new Error(`#${n} has no v0 review`);
  return { state: first.state as string, commit: first.commit_id as string, body: first.body as string };
}

async function ping(url: string, deadline: number) {
  while (Date.now() < deadline) {
    if (await fetch(`${url}/ping`).then(() => true, () => false)) return;
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error('runtime did not start');
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

const pins: Record<string, string> = {};
const v0 = Object.fromEntries(prs.map((n) => [n, v0First(n)]));
for (const n of prs) pins[`${repo}#${n}`] = v0[n].commit;
mkdirSync(outDir, { recursive: true });
const posted = resolve(outDir, 'posted.jsonl');
const service = stubService(pins, posted).listen(0);
await new Promise((r) => service.once('listening', r));
const serviceUrl = `http://127.0.0.1:${(service.address() as AddressInfo).port}`;

for (const [i, n] of prs.entries()) {
  const dir = resolve(outDir, `pr-${n}`);
  mkdirSync(join(dir, 'session'), { recursive: true });
  const port = 18100 + i;
  const env = { ...process.env, QUACK_CONFIG: resolve(deployment), QUACK_AGENT: resolve(agent), QUACK_SESSION_DIR: join(dir, 'session'), PORT: String(port), QUACK_GH_SERVICE_URL: serviceUrl, QUACK_GH_SERVICE_TOKEN: 'bench' };
  const child = spawn(process.execPath, [main], { env, stdio: ['ignore', 'pipe', 'pipe'] });
  const log: string[] = [];
  child.stdout.on('data', (d) => log.push(String(d)));
  child.stderr.on('data', (d) => log.push(String(d)));
  const started = Date.now();
  let result: unknown;
  try {
    await ping(`http://127.0.0.1:${port}`, Date.now() + 30_000);
    result = await untilSettled(`http://127.0.0.1:${port}`, n, started + 30 * 60_000);
  } catch (err) {
    result = { status: 'error', error: String(err) };
  } finally {
    child.kill('SIGTERM');
  }
  const seconds = Math.round((Date.now() - started) / 1000);
  writeFileSync(join(dir, 'result.json'), JSON.stringify({ pr: Number(n), seconds, result, v0: v0[n] }, null, 2));
  writeFileSync(join(dir, 'runtime.log'), log.join(''));
  console.log(`#${n} ${seconds}s ${JSON.stringify(result).slice(0, 160)}`);
}
service.close();
