// Round 2 timing of today's path: reads each session's durable log on prod (read-only) and writes the tool and workflow tasks to out/times.jsonl.
// Usage: node tools/spike-fanout/times.ts [key-prefix]
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';

const prefix = process.argv[2] ?? '';
const rows = JSON.parse(readFileSync('tools/spike-fanout/out/selection.json', 'utf8')) as Array<{ key: string }>;
const keys = [...new Set(rows.map((r) => r.key))].filter((k) => k.startsWith(prefix));
const code = Buffer.from(readFileSync('tools/spike-fanout/extract-times.js', 'utf8')).toString('base64');
const lines: string[] = [];
const BATCH = 40;
for (let i = 0; i < keys.length; i += BATCH) {
  const env = Buffer.from(JSON.stringify(keys.slice(i, i + BATCH))).toString('base64');
  const out = execFileSync('ssh', ['jason-server@jason-server.hs.internal', `docker exec -e KEYS=${env} quack-v1 node -e "eval(Buffer.from('${code}','base64').toString())"`], { encoding: 'utf8', maxBuffer: 256 << 20 });
  lines.push(...out.split('\n').filter(Boolean));
  console.error(`${Math.min(i + BATCH, keys.length)}/${keys.length}`);
}
writeFileSync(`tools/spike-fanout/out/times${prefix ? `-${prefix.replace(/\W+/g, '_')}` : ''}.jsonl`, lines.join('\n'));
