import ts from 'typescript';
import type { Break } from './oasdiff.ts';
import { at, exportsOf, oneLine, program, type Export } from './typescript-rules.ts';

const brk = (id: string, name: string, text: string): Break => ({ id, text, operation: '', path: `/${name}` });
const exportsIn = (src: string) => exportsOf(ts.createSourceFile('x.d.ts', src, ts.ScriptTarget.ES2022, true));

const ref = (ns: string, e: Export, asType: boolean) => (asType ? `${ns}.${e.name}${e.params ? `<${Array(e.params).fill('any').join(', ')}>` : ''}` : `typeof ${ns}.${e.name}`);

type Probe = { name: string; from: 'next' | 'prev' };
const HEAD = ['import type * as prev from "./prev";', 'import type * as next from "./next";'];

// One line per direction (`const x: Target = null as unknown as Source`), so a compiler error's line names its probe.
function probes(shared: Export[]): { lines: string[]; meta: Probe[] } {
  const lines = [...HEAD];
  const meta: Probe[] = [];
  const add = (target: string, source: string, name: string, from: Probe['from']) => {
    lines.push(`const a${meta.length}: ${target} = null as unknown as ${source};`);
    meta.push({ name, from });
  };
  for (const e of shared) {
    for (const asType of [true, false]) {
      if (!(asType ? e.type : e.value)) continue;
      add(ref('prev', e, asType), ref('next', e, asType), e.name, 'next');
      add(ref('next', e, asType), ref('prev', e, asType), e.name, 'prev');
    }
  }
  return { lines, meta };
}

const SIDE = {
  next: ['produced-breaks', 'values core now produces no longer fit old consumers'],
  prev: ['provided-breaks', 'values extensions already provide no longer fit'],
} as const;

/** Breaking changes between two declaration surfaces: removed exports, and each export that stops being assignable in either direction. */
export async function typeScriptBreaks(prev: unknown, next: unknown): Promise<Break[]> {
  if (typeof prev !== 'string' || typeof next !== 'string') return [];
  const before = exportsIn(prev), after = new Set(exportsIn(next).map((e) => e.name));
  const out = [...new Set(before.map((e) => e.name))].filter((n) => !after.has(n)).map((n) => brk('export-removed', n, `export "${n}" is removed, so code that names it no longer compiles`));
  const { lines, meta } = probes(before.filter((e) => after.has(e.name)));
  if (!meta.length) return out;
  const probe = at('probe.ts');
  const p = program({ [at('prev.d.ts')]: prev, [at('next.d.ts')]: next, [probe]: lines.join('\n') }, [probe]);
  const sf = p.getSourceFile(probe)!;
  for (const d of ts.getPreEmitDiagnostics(p, sf)) {
    const m = meta[sf.getLineAndCharacterOfPosition(d.start ?? 0).line - HEAD.length];
    if (m) out.push(brk(SIDE[m.from][0], m.name, `export "${m.name}": ${SIDE[m.from][1]}: ${oneLine(d.messageText)}`));
  }
  return out;
}
