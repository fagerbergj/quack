import type { Context } from '@earendil-works/chord/context';
import type { ToolExecutionApi } from '@earendil-works/pi-durable';

type Range = { start: { line: number; character: number }; end: { line: number; character: number } };
type DocumentSymbol = { name: string; kind: number; range: Range; selectionRange: Range; children?: DocumentSymbol[] };
type SymbolInformation = { name: string; kind: number; location: { uri: string; range: Range } };
type Location = { uri: string; range: Range };
type Changed = { path: string; lines: Array<[number, number]> };
type Symbol = { name: string; kind: number; range: Range; at: Range['start'] };

const MAX_FILES = 20;
const MAX_SYMBOLS = 30;
const CONCURRENCY = 4;
const MAX_LISTED = 8;
const TEST_PATH = /(_test\.go|\.test\.[cm]?[jt]sx?|\.spec\.[cm]?[jt]sx?|(^|\/)(test|tests|__tests__)\/)/;
// The LSP SymbolKinds worth a blast radius; fields, properties and the like ride with their parent.
const KINDS: Record<number, string> = { 5: 'class', 6: 'method', 9: 'constructor', 10: 'enum', 11: 'interface', 12: 'function', 13: 'variable', 14: 'constant', 23: 'struct' };

/** Changed line ranges in the new version (1-based, inclusive) from `git diff -U0`. */
export function changedLines(diff: string): Changed[] {
  const files: Changed[] = [];
  for (const line of diff.split('\n')) {
    const file = /^\+\+\+ b\/(.+)$/.exec(line);
    if (file) files.push({ path: file[1], lines: [] });
    const hunk = /^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@/.exec(line);
    if (hunk && files.length) {
      const start = Number(hunk[1]);
      const count = hunk[2] === undefined ? 1 : Number(hunk[2]);
      files.at(-1)!.lines.push([start, start + Math.max(count, 1) - 1]);
    }
  }
  return files.filter((f) => f.lines.length);
}

const overlaps = (r: Range, lines: Array<[number, number]>) => lines.some(([a, b]) => r.start.line + 1 <= b && r.end.line + 1 >= a);

// Flat SymbolInformation ranges start at the declaration (`export function`), so the name's own position is found in the text.
function namePosition(s: SymbolInformation, text: string): Range['start'] {
  const { line, character } = s.location.range.start;
  const at = (text.split('\n')[line] ?? '').indexOf(s.name, character);
  return { line, character: at < 0 ? character : at };
}

/** The innermost named symbols whose range a change touches. */
export function touchedSymbols(result: unknown, lines: Array<[number, number]>, text = ''): Symbol[] {
  if (!Array.isArray(result)) return [];
  const out: Symbol[] = [];
  const visit = (s: DocumentSymbol) => {
    if (!overlaps(s.range, lines)) return;
    const inner = (s.children ?? []).filter((c) => overlaps(c.range, lines) && KINDS[c.kind]);
    if (inner.length) return inner.forEach(visit);
    if (KINDS[s.kind]) out.push({ name: s.name, kind: s.kind, range: s.range, at: s.selectionRange.start });
  };
  for (const s of result as Array<DocumentSymbol | SymbolInformation>) {
    if ('location' in s) {
      if (KINDS[s.kind] && overlaps(s.location.range, lines)) out.push({ name: s.name, kind: s.kind, range: s.location.range, at: namePosition(s, text) });
    } else visit(s);
  }
  return out;
}

function describe(sym: Symbol, file: string, refs: Location[] | string, root: string): string {
  if (typeof refs === 'string') return `- ${file}:${sym.at.line + 1} ${KINDS[sym.kind]} ${sym.name}: references unavailable: ${refs.replace(/\s+/g, ' ').slice(0, 200)}`;
  const outside = refs
    .map((r) => ({ path: decodeURIComponent(r.uri.replace(/^file:\/\//, '')).replace(`${root}/`, ''), line: r.range.start.line + 1 }))
    .filter((r) => !(r.path === file && r.line >= sym.range.start.line + 1 && r.line <= sym.range.end.line + 1));
  const tests = outside.filter((r) => TEST_PATH.test(r.path));
  const callers = outside.filter((r) => !TEST_PATH.test(r.path));
  const list = (xs: typeof outside) => xs.slice(0, MAX_LISTED).map((r) => `${r.path}:${r.line}`).join(', ') + (xs.length > MAX_LISTED ? `, +${xs.length - MAX_LISTED} more` : '');
  const head = `- ${file}:${sym.at.line + 1} ${KINDS[sym.kind]} ${sym.name}`;
  if (!outside.length) return `${head}: no references outside itself`;
  return `${head}: ${callers.length} references${callers.length ? ` (${list(callers)})` : ''}; ${tests.length ? `tests: ${list(tests)}` : 'no tests reference it'}`;
}

async function lsp(api: ToolExecutionApi, tool: string, args: Record<string, unknown>, key: string, ctx: Context): Promise<unknown> {
  const r = await api.executeTool(tool, args as never, ctx, { key });
  return r.isError ? undefined : r.structuredOutput;
}

/** Maps in order with at most CONCURRENCY calls in flight. */
async function pool<T, R>(items: T[], fn: (item: T, i: number) => Promise<R>): Promise<R[]> {
  const out: R[] = new Array(items.length);
  let next = 0;
  const worker = async () => { for (let i = next++; i < items.length; i = next++) out[i] = await fn(items[i], i); };
  await Promise.all(Array.from({ length: Math.min(CONCURRENCY, items.length) }, worker));
  return out;
}

// An error, a timeout or any non-list answer is "unknown", never "no callers"; the server's own "No result" is an empty answer.
async function references(api: ToolExecutionApi, path: string, at: Range['start'], key: string, ctx: Context): Promise<Location[] | string> {
  const r = await api.executeTool('lsp_references', { path, line: at.line, character: at.character, includeDeclaration: false } as never, ctx, { key });
  const out = r.isError ? (typeof r.structuredOutput === 'string' && r.structuredOutput) || r.diagnostics.map((d) => d.message).join(' ') || 'tool error' : r.structuredOutput;
  if (Array.isArray(out)) return out as unknown as Location[];
  return out === 'No result' ? [] : String(out);
}

// GH-3: who depends on each changed symbol, from the language servers; empty when the session has no LSP tools.
// Explicit call keys keep a replay matching its nested calls whatever order the parallel ones finish in.
export async function blastRadius(api: ToolExecutionApi, repoDir: string, diff: string, ctx: Context): Promise<string> {
  const names = new Set((await api.agent(ctx)).callable.map((t) => t.name));
  if (!names.has('lsp_symbols') || !names.has('lsp_references')) return '';
  const root = `${api.env!.cwd}/${repoDir}`;
  const all = changedLines(diff);
  const files = all.slice(0, MAX_FILES);
  const perFile = await pool(files, async (file, i) => {
    const text = await api.env!.readTextFile(`${repoDir}/${file.path}`, ctx);
    const symbols = touchedSymbols(await lsp(api, 'lsp_symbols', { path: `${repoDir}/${file.path}` }, `sym:${i}`, ctx), file.lines, text.ok ? text.value : '');
    return symbols.map((sym) => ({ sym, file: file.path }));
  });
  const found = perFile.flat();
  const kept = found.slice(0, MAX_SYMBOLS);
  const lines = await pool(kept, async ({ sym, file }, i) => describe(sym, file, await references(api, `${repoDir}/${file}`, sym.at, `ref:${i}`, ctx), root));
  const capped = [all.length > files.length && `${files.length} of ${all.length} changed files`, found.length > kept.length && `${kept.length} of ${found.length} changed symbols`].filter(Boolean);
  if (capped.length) lines.push(`Blast radius covers ${capped.join(' and ')}; the rest were not checked.`);
  return lines.length ? `Blast radius of changed symbols:\n${lines.join('\n')}` : '';
}
