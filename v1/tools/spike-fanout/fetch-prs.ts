// Round 2: PR text, diff, function-context diff and added-file siblings for the PRs of the failed review versions.
// Usage: node tools/spike-fanout/fetch-prs.ts <data-dir> <pr>...
import { spawnSync } from 'node:child_process';
import { existsSync, readFileSync, writeFileSync } from 'node:fs';

const [dir, ...nums] = process.argv.slice(2);
const run = (cmd: string, args: string[]) => { const r = spawnSync(cmd, args, { encoding: 'utf8', maxBuffer: 128 << 20 }); return r.status === 0 ? r.stdout : ''; };
const MAX_SIBLING_CHARS = 12_000;
const MAX_SIBLINGS = 12;

for (const n of nums) {
  const f = `${dir}/pr-${n}.json`;
  if (existsSync(f)) continue;
  const view = run('gh', ['pr', 'view', n, '-R', 'fagerbergj/quack', '--json', 'title,body,closingIssuesReferences']);
  const diff = run('gh', ['pr', 'diff', n, '-R', 'fagerbergj/quack']);
  if (!view || !diff) { console.error(`#${n}: no PR data`); continue; }
  const pr = JSON.parse(view);
  run('git', ['fetch', '-q', 'origin', `pull/${n}/head:refs/spike/${n}`]);
  const base = run('git', ['merge-base', 'origin/v1', `refs/spike/${n}`]).trim();
  const diffW = base ? run('git', ['diff', '--function-context', base, `refs/spike/${n}`]) : '';
  const repoFiles: Array<{ path: string; ts: number; text: string }> = [];
  for (const m of diff.matchAll(/^diff --git a\/(\S+) b\/\S+\nnew file mode/gm)) {
    const path = m[1];
    const dirName = path.includes('/') ? path.slice(0, path.lastIndexOf('/') + 1) : '';
    const ext = path.slice(path.lastIndexOf('.'));
    for (const p of run('git', ['ls-tree', '--full-tree', '--name-only', base, dirName || '.']).split('\n').filter((x) => x.endsWith(ext) && x !== path).slice(0, MAX_SIBLINGS)) {
      if (repoFiles.some((r) => r.path === p)) continue;
      repoFiles.push({ path: p, ts: Number(run('git', ['log', '-1', '--format=%ct', base, '--', p]).trim() || 0), text: run('git', ['show', `${base}:${p}`]).slice(0, MAX_SIBLING_CHARS) });
    }
  }
  writeFileSync(f, JSON.stringify({ n: Number(n), pr: { title: pr.title, body: pr.body }, issues: [], diff, diffW, repoFiles }));
  console.error(`#${n} diff=${diff.length} added siblings=${repoFiles.length}`);
}
void readFileSync;
