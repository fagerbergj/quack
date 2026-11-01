import { readdir, readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { digest } from '../packages/distributor/src/index.ts';

const root = new URL('../agents/', import.meta.url).pathname;

/** Name, version and content hash of every bundle under v1/agents, as its version.lock should read. */
export async function bundleLocks() {
  const dirs = (await readdir(root, { withFileTypes: true })).filter((e) => e.isDirectory());
  return Promise.all(dirs.map(async (d) => {
    const dir = join(root, d.name);
    const { name, version } = JSON.parse(await readFile(join(dir, 'agent.json'), 'utf8'));
    return { dir, lock: `${JSON.stringify({ name, version, hash: await digest(dir) }, null, 2)}\n` };
  }));
}

// `node tools/bundle-locks.ts` rewrites every lock; run it only together with a version bump.
if (process.argv[1] === new URL(import.meta.url).pathname) for (const { dir, lock } of await bundleLocks()) await writeFile(join(dir, 'version.lock'), lock);
