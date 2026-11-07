// Usage: node tools/memory-spike/episodes.ts <sessions-dir> > episodes.jsonl
// One raw episode per copied session, built in code (no model): its inputs, the sealed versions the judge passed, and its final answer.
import { readdir, readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { readSession } from '@quack/eval';

export type Episode = { key: string; created: string; title: string; text: string };

export async function episodeOf(dir: string): Promise<Episode> {
  const r = await readSession(dir);
  const key = (await readFile(join(dir, 'session-key'), 'utf8')).trim();
  const created = new Date(Number(await readFile(join(dir, 'created'), 'utf8'))).toISOString();
  const title = (await readFile(join(dir, 'title'), 'utf8').catch(() => '')).trim();
  const inputs = r.inputs.map((t, i) => `### Request ${i + 1}\n${t.trim()}`);
  const sealed = Object.entries(r.artifacts).flatMap(([kind, vs]) => vs.filter((v) => (v.verdict as { passed?: boolean } | undefined)?.passed !== false).map((v) => `### ${kind} v${v.version}\n${String(v.content).trim()}`));
  const answer = r.outcome.status === 'done' ? [`### Final answer\n${r.outcome.answer.trim()}`] : [`### Outcome\n${r.outcome.status}`];
  return { key, created, title, text: [`# ${key} (${created.slice(0, 16)}Z): ${title}`, ...inputs, ...sealed, ...answer].join('\n\n') };
}

if (import.meta.main) {
  const root = process.argv[2];
  const all = await Promise.all((await readdir(root)).map((d) => episodeOf(join(root, d))));
  for (const e of all.sort((a, b) => a.created.localeCompare(b.created))) console.log(JSON.stringify(e));
}
