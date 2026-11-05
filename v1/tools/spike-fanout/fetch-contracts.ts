// Pulls the latest contract.spec of each contracts session from prod, read-only. Usage: node tools/spike-fanout/fetch-contracts.ts
import { readFileSync, writeFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';

const sessions = JSON.parse(readFileSync('tools/spike-fanout/data/cs.json', 'utf8')).sessions as Array<{ key: string }>;
for (const s of sessions) {
  const cmd = `docker exec -e QUACK_URL=http://localhost:8080 quack-v1 sh -c 'cd /app && node packages/cli/src/main.ts --json artifact "${s.key}" contract.spec'`;
  const r = spawnSync('ssh', ['jason-server@jason-server.hs.internal', cmd], { encoding: 'utf8', maxBuffer: 64 << 20 });
  if (r.status !== 0) { console.error(`${s.key}: none`); continue; }
  writeFileSync(`tools/spike-fanout/data/contract-${s.key.replace(/\W+/g, '_')}.json`, r.stdout);
  console.error(`${s.key}: ${r.stdout.length} chars`);
}
