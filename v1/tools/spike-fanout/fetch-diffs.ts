// Adds a function-context diff and the base-side siblings of each added file to the fetched review records.
// Usage: node tools/spike-fanout/fetch-diffs.ts <data-dir> <pr number>...
import { spawnSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';

const [dir, ...nums] = process.argv.slice(2);
const git = (...a: string[]) => { const r = spawnSync('git', a, { encoding: 'utf8', maxBuffer: 64 << 20 }); return r.status === 0 ? r.stdout : ''; };
const MAX_SIBLING_CHARS = 12_000;
const MAX_SIBLINGS = 12;

for (const n of nums) {
  const f = `${dir}/review-${n}.json`;
  const rec = JSON.parse(readFileSync(f, 'utf8'));
  git('fetch', '-q', 'origin', `pull/${n}/head:refs/spike/${n}`);
  const base = git('merge-base', 'origin/v1', `refs/spike/${n}`).trim();
  rec.diffW = base ? git('diff', '--function-context', base, `refs/spike/${n}`) : '';
  const added = [...rec.diff.matchAll(/^diff --git a\/(\S+) b\/\S+\nnew file mode/gm)].map((m) => m[1]);
  rec.repoFiles = [];
  for (const path of added) {
    const dirName = path.includes('/') ? path.slice(0, path.lastIndexOf('/') + 1) : '';
    const ext = path.slice(path.lastIndexOf('.'));
    for (const p of git('ls-tree', '--full-tree', '--name-only', base, dirName || '.').split('\n').filter((x) => x.endsWith(ext) && x !== path).slice(0, MAX_SIBLINGS)) {
      if (rec.repoFiles.some((r: { path: string }) => r.path === p)) continue;
      const ts = Number(git('log', '-1', '--format=%ct', base, '--', p).trim() || 0);
      rec.repoFiles.push({ path: p, ts, text: git('show', `${base}:${p}`).slice(0, MAX_SIBLING_CHARS) });
    }
  }
  writeFileSync(f, JSON.stringify(rec));
  console.error(`#${n} base=${base.slice(0, 8)} diffW=${rec.diffW.length} added=${added.length} siblings=${rec.repoFiles.length}`);
}
