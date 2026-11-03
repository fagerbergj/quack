import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

/** Package layers (RFC extension dependencies): arrows point down only; services and tools sit beside the stack as leaves. */
export const LAYERS: Record<string, 'core' | 'foundation' | 'feature' | 'leaf'> = {
  sdk: 'core', session: 'core', index: 'core', distributor: 'core', runtime: 'core', web: 'core',
  judge: 'foundation', workflows: 'foundation', slots: 'foundation', traces: 'foundation',
  github: 'feature', home: 'feature', sleeper: 'feature', remarkable: 'feature', research: 'feature', contracts: 'feature',
  schedules: 'leaf', eval: 'leaf', cli: 'leaf',
};

/** Known violations to retire, as 'from -> to': issue. Empty today; a stale entry fails the test. */
export const ALLOWED: Record<string, string> = {};

const CAN_IMPORT = { core: ['core'], foundation: ['core', 'foundation'], feature: ['core', 'foundation'], leaf: ['core', 'foundation'] } as const;

/** Why `from` may not import `to`, or undefined. Runtime assembles every package; only core and leaves may import it back. */
export function arrowProblem(from: string, to: string): string | undefined {
  if (from === to || from === 'runtime') return undefined;
  const [a, b] = [LAYERS[from], LAYERS[to]];
  if (!a || !b) return `${!a ? from : to} has no layer in tools/boundary.ts`;
  if (to === 'runtime' ? a === 'core' || a === 'leaf' : (CAN_IMPORT[a] as readonly string[]).includes(b)) return undefined;
  return `${from} (${a}) must not import ${to} (${b})`;
}

type Edge = { from: string; to: string; via: 'src' | 'package.json' };

/** Each package's @quack imports from src (test files excluded) and its runtime dependencies. */
export function packageEdges(packagesDir: string): Edge[] {
  const edges: Edge[] = [];
  for (const dir of readdirSync(packagesDir)) {
    const pkg = JSON.parse(readFileSync(join(packagesDir, dir, 'package.json'), 'utf8'));
    for (const name of Object.keys({ ...pkg.dependencies, ...pkg.peerDependencies })) if (name.startsWith('@quack/')) edges.push({ from: dir, to: name.slice(7), via: 'package.json' });
    for (const file of readdirSync(join(packagesDir, dir, 'src'), { recursive: true, encoding: 'utf8' }).filter((f) => /\.tsx?$/.test(f))) {
      for (const [, to] of readFileSync(join(packagesDir, dir, 'src', file), 'utf8').matchAll(/(?:from |import\()'@quack\/([a-z-]+)/g)) edges.push({ from: dir, to, via: 'src' });
    }
  }
  return edges.filter((e) => e.from !== e.to);
}

/** Boundary problems, minus allow-listed arrows; allow-list entries that no longer occur are problems too. */
export function boundaryProblems(edges: Edge[], allowed = ALLOWED): string[] {
  const seen = new Set<string>();
  const problems = edges.flatMap((e) => {
    const why = arrowProblem(e.from, e.to);
    if (!why) return [];
    seen.add(`${e.from} -> ${e.to}`);
    return `${e.from} -> ${e.to}` in allowed ? [] : [`${why} (${e.via})`];
  });
  return [...problems, ...Object.keys(allowed).filter((k) => !seen.has(k)).map((k) => `allow-list entry ${k} (${allowed[k]}) is no longer a violation; remove it`)];
}
