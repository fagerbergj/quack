// Import cycles a diff adds or grows: runtime imports between files of one package, by file and by directory.
import { execFileSync } from 'node:child_process';
import { dirname, posix } from 'node:path';
import ts from 'typescript';

/** A tie from `from`:`line` to `to`; `step` says it in a finding, the import by default. */
export type Edge = { from: string; to: string; line: number; step?: string };
export type CycleFinding = { file: string; line: number; message: string };
/** By group (a package or directory), each node's ties to the next, at their first site. */
export type Graph = Map<string, Map<string, Map<string, Edge>>>;

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

/** Each runtime import between two files of one package (`packages/<name>/`) in `sources`. */
export const pkg = (f: string) => /^packages\/[^/]+\//.exec(f)?.[0];

/** The file in `sources` a relative specifier in `from` names. */
export const resolver = (sources: Map<string, string>) => (from: string, spec: string) => {
  const p = posix.join(dirname(from), spec);
  return [p, `${p}.ts`, `${p}.tsx`, `${p}/index.ts`, `${p}/index.tsx`].find((c) => sources.has(c));
};

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
function graphOf(edges: Edge[]): Graph {
  const g: Graph = new Map();
  for (const e of edges) for (const [group, x, y] of ties(e)) addTie(g, group, x, y, e);
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

/** Import cycles in `head` that hold an import `base` lacks. */
export const cycleFindings = (base: Edge[], head: Edge[]) =>
  gate(graphOf(base), graphOf(head), (group, c) => `import cycle across ${c.length} ${group.startsWith('dir') ? `entries of ${group.slice(4)}/` : 'files'}`);

/** Each file in a file-level import cycle, by the cycle it is in. */
export function fileCycles(edges: Edge[]): Map<string, number> {
  const g: Graph = new Map();
  edges.forEach((e) => addTie(g, 'files', e.from, e.to, e));
  return new Map(components(g.get('files') ?? new Map()).flatMap((c, i) => c.map((f) => [f, i] as const)));
}

/** The sources as they stood at the merge base with `ref`: `head` with every file the diff names read back from there. */
export function baseSources(root: string, ref: string, head: Map<string, string>, keep: (f: string) => boolean): Map<string, string> {
  const git = (args: string[], input?: string) => execFileSync('git', args, { cwd: root, input, maxBuffer: 1 << 28 });
  const at = git(['merge-base', ref, 'HEAD']).toString().trim();
  const named = git(['diff', '--name-only', '--no-renames', '-z', `${at}...HEAD`]).toString().split('\0').filter((f) => f && keep(f));
  const base = new Map(head);
  named.forEach((f) => base.delete(f));
  if (!named.length) return base;
  const out = git(['cat-file', '--batch'], named.map((f) => `${at}:${f}\n`).join(''));
  for (let pos = 0, i = 0; i < named.length; i++) {
    const eol = out.indexOf(10, pos);
    const header = out.subarray(pos, eol).toString();
    pos = eol + 1;
    if (header.endsWith(' missing')) continue;
    const size = Number(header.split(' ')[2]);
    base.set(named[i], out.subarray(pos, pos + size).toString('utf8'));
    pos += size + 1;
  }
  return base;
}
