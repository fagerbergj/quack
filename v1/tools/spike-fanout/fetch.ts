// Pulls the spike's data read-only: github.review and research.report artifacts from prod, plus each PR's text and diff from gh.
// Usage: node tools/spike-fanout/fetch.ts <out-dir> [max-reviews]
import { spawnSync } from 'node:child_process';
import { mkdirSync, writeFileSync } from 'node:fs';

const [out = 'data', maxArg = '40'] = process.argv.slice(2);
mkdirSync(out, { recursive: true });

function prod(...args: string[]): any {
  const cmd = `docker exec -e QUACK_URL=http://localhost:8080 quack-v1 sh -c 'cd /app && node packages/cli/src/main.ts --json ${args.map((a) => `"${a}"`).join(' ')}'`;
  const r = spawnSync('ssh', ['jason-server@jason-server.hs.internal', cmd], { encoding: 'utf8', maxBuffer: 64 << 20 });
  if (r.status !== 0) return undefined;
  try { return JSON.parse(r.stdout); } catch { return undefined; }
}
function gh(...args: string[]): string | undefined {
  const r = spawnSync('gh', args, { encoding: 'utf8', maxBuffer: 64 << 20 });
  return r.status === 0 ? r.stdout : undefined;
}

type Sess = { key: string; agent: string };
const reviews: Sess[] = prod('sessions', '--agent', 'reviewer').sessions.filter((s: Sess) => /^github:fagerbergj\/quack#\d+$/.test(s.key));
const kept: unknown[] = [];
for (const s of reviews.slice(0, Number(maxArg))) {
  const n = Number(s.key.split('#')[1]);
  const art = prod('artifact', s.key, 'github.review');
  if (!art?.verdict) continue;
  const checks: Array<{ id: string; resolvers: Record<string, number> }> = art.verdict.checks;
  const esc = ['promises_handled', 'sibling_parts', 'no_silent_approval', 'reports_every_defect'].filter((id) => Object.keys(checks.find((c) => c.id === id)?.resolvers ?? {}).some((r) => r !== 'clef' && r !== 'code'));
  const view = gh('pr', 'view', String(n), '-R', 'fagerbergj/quack', '--json', 'title,body,closingIssuesReferences,files,baseRefName,headRefOid');
  const diff = gh('pr', 'diff', String(n), '-R', 'fagerbergj/quack');
  if (!view || !diff) continue;
  const pr = JSON.parse(view);
  const issues: unknown[] = [];
  for (const i of pr.closingIssuesReferences ?? []) {
    const t = gh('issue', 'view', String(i.number), '-R', 'fagerbergj/quack', '--json', 'number,title,body');
    if (t) issues.push(JSON.parse(t));
  }
  const rec = { n, key: s.key, review: art.content, verdict: art.verdict, escalated: esc, pr: { title: pr.title, body: pr.body, files: pr.files, base: pr.baseRefName, head: pr.headRefOid }, issues, diff };
  writeFileSync(`${out}/review-${n}.json`, JSON.stringify(rec));
  kept.push({ n, rubric: art.verdict.rubric, esc });
  console.error(`#${n} rubric=${art.verdict.rubric} escalated=${esc.join(',')}`);
}
const reports: unknown[] = [];
for (const s of prod('sessions', '--agent', 'researcher').sessions as Sess[]) {
  const art = prod('artifact', s.key, 'research.report');
  if (!art?.content) continue;
  const first = prod('show', s.key);
  writeFileSync(`${out}/report-${s.key.replace(/\W+/g, '_')}.json`, JSON.stringify({ key: s.key, report: art.content, verdict: art.verdict, show: first }));
  reports.push(s.key);
  console.error(`report ${s.key}`);
}
writeFileSync(`${out}/index.json`, JSON.stringify({ reviews: kept, reports }));
