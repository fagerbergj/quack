import { readdir, readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { bundleProblems } from '../packages/session/src/index.ts';
import { digest } from '../packages/distributor/src/index.ts';

const root = new URL('../agents/', import.meta.url).pathname;

/** Name, version and content hash of every bundle under agents/, as its version.lock should read. */
export async function bundleLocks() {
  const dirs = (await readdir(root, { withFileTypes: true })).filter((e) => e.isDirectory());
  return Promise.all(dirs.map(async (d) => {
    const dir = join(root, d.name);
    const def = JSON.parse(await readFile(join(dir, 'agent.json'), 'utf8'));
    const bad = bundleProblems(def);
    if (bad.length) throw new Error(`${dir}/agent.json breaks the agent bundle contract:\n- ${bad.join('\n- ')}`);
    const { name, version } = def;
    return { dir, lock: `${JSON.stringify({ name, version, hash: await digest(dir) }, null, 2)}\n` };
  }));
}

// `node tools/bundle-locks.ts` rewrites every lock; run it only together with a version bump.
if (process.argv[1] === new URL(import.meta.url).pathname) for (const { dir, lock } of await bundleLocks()) await writeFile(join(dir, 'version.lock'), lock);
