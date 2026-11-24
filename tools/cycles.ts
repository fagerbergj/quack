// Import cycles a diff adds or grows: runtime imports between files of one package, by file and by directory.
import { execFileSync } from 'node:child_process';
import { dirname, posix } from 'node:path';
import ts from 'typescript';

/** A tie from `from`:`line` to `to`; `step` says it in a finding, the import by default. */
export type Edge = { from: string; to: string; line: number; step?: string };
export type CycleFinding = { file: string; line: number; message: string };
/** By group (a package or directory), each node's ties to the next, at their first site. */
export type Graph = Map<string, Map<string, Map<string, Edge>>>;
/** The merge base's sources under their own paths, and where the diff moved each moved file. */
export type Base = { sources: Map<string, string>; moved: Map<string, string> };

/** Ties of the base, with moved files under the paths they moved to, so a rename adds no tie. */
export const moveEdges = <E extends Edge>(edges: E[], moved: Map<string, string>): E[] =>
  edges.map((e) => ({ ...e, from: moved.get(e.from) ?? e.from, to: moved.get(e.to) ?? e.to }));

const specsCache = new Map<string, { text: string; specs: Array<{ spec: string; line: number }> }>();

/** Relative runtime imports: `import type`, all-`type` specifiers, re-exports and dynamic imports erase or load late. */
function runtimeSpecs(path: string, text: string) {
  const hit = specsCache.get(path);
  if (hit?.text === text) return hit.specs;
  const sf = ts.createSourceFile(path, text, ts.ScriptTarget.Latest, false, path.endsWith('x') ? ts.ScriptKind.TSX : ts.ScriptKind.TS);
  const specs = sf.statements.flatMap((s) => {
    if (!ts.isImportDeclaration(s) || !ts.isStringLiteral(s.moduleSpecifier) || !s.moduleSpecifier.text.startsWith('.')) return [];
    const c = s.importClause;
    const named = c?.namedBindings && ts.isNamedImports(c.namedBindings) ? c.namedBindings.elements : undefined;
    if (c?.isTypeOnly || (c && !c.name && named?.length && named.every((e) => e.isTypeOnly))) return [];
    return [{ spec: s.moduleSpecifier.text, line: sf.getLineAndCharacterOfPosition(s.getStart(sf)).line + 1 }];
  });
  specsCache.set(path, { text, specs });
  return specs;
}

export const pkg = (f: string) => /^packages\/[^/]+\//.exec(f)?.[0];

/** The file in `sources` a relative specifier in `from` names. */
export const resolver = (sources: Map<string, string>) => (from: string, spec: string) => {
  const p = posix.join(dirname(from), spec);
  return [p, `${p}.ts`, `${p}.tsx`, `${p}/index.ts`, `${p}/index.tsx`].find((c) => sources.has(c));
};

/** Each runtime import between two files of one package (`packages/<name>/`) in `sources`. */
export function importEdges(sources: Map<string, string>): Edge[] {
  const resolve = resolver(sources);
  return [...sources].flatMap(([from, text]) => {
    if (!pkg(from)) return [];
    return runtimeSpecs(from, text).flatMap(({ spec, line }) => {
      const to = resolve(from, spec);
      return to && to !== from && pkg(to) === pkg(from) ? [{ from, to, line }] : [];
    });
  });
}

/** Where an edge ties two nodes: per package between files, and under the directory both share between its children. */
function ties(e: Edge): Array<[string, string, string]> {
  const [a, b] = [e.from.split('/'), e.to.split('/')];
  let i = 0;
  while (i < a.length - 1 && i < b.length - 1 && a[i] === b[i]) i++;
  const top = a.slice(0, i + 1).join('/') === e.from && b.slice(0, i + 1).join('/') === e.to;
  const byFile: [string, string, string] = [`file ${a.slice(0, 2).join('/')}`, e.from, e.to];
  return top ? [byFile] : [byFile, [`dir ${a.slice(0, i).join('/')}`, a.slice(0, i + 1).join('/'), b.slice(0, i + 1).join('/')]];
}

export function addTie(g: Graph, group: string, x: string, y: string, e: Edge) {
  const nodes = g.get(group) ?? g.set(group, new Map()).get(group)!;
  const out = nodes.get(x) ?? nodes.set(x, new Map()).get(x)!;
  if (!out.has(y) || e.line < out.get(y)!.line) out.set(y, e);
}

// File-level groups come first, so a change that shows at both levels reports the precise one.
const entry = (dir: string, child: string) => `${posix.relative(dir, child)}${/\.tsx?$/.test(child) ? '' : '/'}`;

function graphOf(edges: Edge[]): Graph {
  const g: Graph = new Map();
  for (const e of edges) for (const [group, x, y] of ties(e)) {
    const dir = group.slice(4);
    addTie(g, group, x, y, group.startsWith('dir') ? { ...e, step: `${posix.relative(dir, e.from)}:${e.line} → ${entry(dir, y)}` } : e);
  }
  return new Map([...g].sort(([a], [b]) => Number(a.startsWith('dir')) - Number(b.startsWith('dir'))));
}

/** Strongly connected components of two or more nodes (iterative Tarjan). */
export function components(nodes: Map<string, Map<string, Edge>>): string[][] {
  const [index, low, onStack, stack, out] = [new Map<string, number>(), new Map<string, number>(), new Set<string>(), [] as string[], [] as string[][]];
  for (const root of nodes.keys()) {
    if (index.has(root)) continue;
    const work: Array<[string, string[]]> = [];
    const enter = (v: string) => { index.set(v, index.size); low.set(v, index.get(v)!); stack.push(v); onStack.add(v); work.push([v, [...(nodes.get(v)?.keys() ?? [])]]); };
    enter(root);
    while (work.length) {
      const [v, next] = work.at(-1)!;
      const w = next.pop();
      if (w !== undefined && !index.has(w)) enter(w);
      else if (w !== undefined) { if (onStack.has(w)) low.set(v, Math.min(low.get(v)!, index.get(w)!)); }
      else {
        work.pop();
        if (work.length) low.set(work.at(-1)![0], Math.min(low.get(work.at(-1)![0])!, low.get(v)!));
        if (low.get(v) !== index.get(v)) continue;
        const c: string[] = [];
        for (let x = stack.pop()!; ; x = stack.pop()!) { onStack.delete(x); c.push(x); if (x === v) break; }
        if (c.length > 1) out.push(c);
      }
    }
  }
  return out;
}

/** The shortest way round the component that starts with `first` (x → y), as the import behind each step. */
function circle(nodes: Map<string, Map<string, Edge>>, members: Set<string>, first: Edge, [x, y]: [string, string]): Edge[] {
  const via = new Map<string, string>([[y, y]]);
  for (const queue = [y]; queue.length && !via.has(x);) {
    const at = queue.shift()!;
    for (const n of nodes.get(at)?.keys() ?? []) if (members.has(n) && !via.has(n)) { via.set(n, at); queue.push(n); }
  }
  const path: Edge[] = [];
  for (let at = x; at !== y; at = via.get(at)!) path.unshift(nodes.get(via.get(at)!)!.get(at)!);
  return [first, ...path];
}

/** One finding per component in `head` that `name` names and that holds a tie `old` lacks, at that new tie. */
export function gate(old: Graph, head: Graph, name: (group: string, members: string[]) => string | undefined): CycleFinding[] {
  const seen = new Set<string>();
  const found: CycleFinding[] = [];
  for (const [group, nodes] of head) for (const c of components(nodes)) {
    const what = name(group, c);
    if (!what) continue;
    const members = new Set(c);
    const fresh = c.flatMap((x) => [...nodes.get(x)!].filter(([y]) => members.has(y) && !old.get(group)?.get(x)?.has(y)).map(([y, e]) => ({ x, y, e })));
    fresh.sort((p, q) => p.e.from.localeCompare(q.e.from) || p.e.line - q.e.line);
    const first = fresh.find(({ e }) => !seen.has(`${e.from}:${e.line}`));
    if (!first) continue;
    fresh.forEach(({ e }) => seen.add(`${e.from}:${e.line}`));
    const steps = circle(nodes, members, first.e, [first.x, first.y]).map((e) => e.step ?? `${e.from}:${e.line} → ${e.to}`);
    found.push({ file: first.e.from, line: first.e.line, message: `${what}: ${steps.join(', ')}` });
  }
  return found;
}

const cycleName = (group: string, c: string[]) => {
  const dir = group.slice(4);
  return group.startsWith('dir') ? `import cycle across ${c.length} entries of ${dir}/ (${c.map((x) => entry(dir, x)).sort().join(', ')})` : `import cycle across ${c.length} files`;
};

/** Import cycles in `head` that hold an import the base lacks. */
export const cycleFindings = (base: Base, head: Map<string, string>) =>
  gate(graphOf(moveEdges(importEdges(base.sources), base.moved)), graphOf(importEdges(head)), cycleName);

/** Each file in a file-level import cycle, by the cycle it is in. */
export function fileCycles(edges: Edge[]): Map<string, number> {
  const g: Graph = new Map();
  edges.forEach((e) => addTie(g, 'files', e.from, e.to, e));
  return new Map(components(g.get('files') ?? new Map()).flatMap((c, i) => c.map((f) => [f, i] as const)));
}

/** The sources at the merge base with `ref`: `head` with each file the diff names read back from there, and the diff's moves. */
export function baseSources(root: string, ref: string, head: Map<string, string>, keep: (f: string) => boolean): Base {
  const git = (args: string[], input?: string) => execFileSync('git', args, { cwd: root, input, maxBuffer: 1 << 28 });
  const at = git(['merge-base', ref, 'HEAD']).toString().trim();
  const fields = git(['diff', '--name-status', '--find-renames', '-l0', '-z', `${at}...HEAD`]).toString().split('\0');
  const [sources, moved, old] = [new Map(head), new Map<string, string>(), [] as string[]];
  for (let i = 0; i < fields.length - 1;) {
    const status = fields[i];
    const paths = status.startsWith('R') || status.startsWith('C') ? fields.slice(i + 1, i + 3) : [fields[i + 1]];
    i += paths.length + 1;
    const [from, to] = [paths[0], paths.at(-1)!];
    if (keep(to)) sources.delete(to);
    if (status.startsWith('R') && keep(from) && keep(to)) moved.set(from, to);
    if (!status.startsWith('A') && keep(from)) old.push(from);
  }
  if (!old.length) return { sources, moved };
  const out = git(['cat-file', '--batch'], old.map((f) => `${at}:${f}\n`).join(''));
  for (let pos = 0, i = 0; i < old.length; i++) {
    const eol = out.indexOf(10, pos);
    const header = out.subarray(pos, eol).toString();
    pos = eol + 1;
    if (header.endsWith(' missing')) continue;
    const size = Number(header.split(' ')[2]);
    sources.set(old[i], out.subarray(pos, pos + size).toString('utf8'));
    pos += size + 1;
  }
  return { sources, moved };
}
