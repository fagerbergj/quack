// Round 2: every version of every judged artifact in the sessions of interest, read-only, with its verdict.
// Usage: node tools/spike-fanout/fetch-all.ts <out-dir>
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { mkdirSync, writeFileSync } from 'node:fs';

const run = promisify(execFile);
const out = process.argv[2] ?? 'tools/spike-fanout/data/all';
mkdirSync(out, { recursive: true });

async function prod(...args: string[]): Promise<any> {
  const cmd = `docker exec -e QUACK_URL=http://localhost:8080 quack-v1 sh -c 'cd /app && node packages/cli/src/main.ts --json ${args.map((a) => `"${a}"`).join(' ')}'`;
  try { return JSON.parse((await run('ssh', ['jason-server@jason-server.hs.internal', cmd], { maxBuffer: 256 << 20 })).stdout); } catch { return undefined; }
}

const sessions: Array<{ key: string; agent: string }> = (await prod('sessions')).sessions;
const keys = sessions.filter((s) => /^github:/.test(s.key) || s.key.startsWith('sleeper:') || ['researcher', 'contracts'].includes(s.agent)).map((s) => s.key);
console.error(`${sessions.length} sessions, ${keys.length} to read`);
let next = 0;
async function worker() {
  while (next < keys.length) {
    const key = keys[next++];
    const r = await prod('artifacts', key);
    if (r?.artifacts?.length) writeFileSync(`${out}/${key.replace(/\W+/g, '_')}.json`, JSON.stringify({ key, artifacts: r.artifacts }));
    else console.error(`${key}: none`);
  }
}
await Promise.all([worker(), worker(), worker(), worker()]);
