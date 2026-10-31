import { setTimeout as sleep } from 'node:timers/promises';
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
const MAX_LISTED = 8;
const WARMUP_MS = 10_000;
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

function describe(sym: Symbol, file: string, refs: Location[], root: string): string {
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

async function lsp(api: ToolExecutionApi, tool: string, args: Record<string, unknown>, ctx: Context): Promise<unknown> {
  const r = await api.executeTool(tool, args as never, ctx);
  return r.isError ? undefined : r.structuredOutput;
}

// A server still loading its project answers from open files only, and LSP has no ready signal through pi-lsp; so until one
// answer reaches another file, or the warm-up passes, a same-file-only answer is asked again.
function referenceFinder(api: ToolExecutionApi, ctx: Context) {
  const until = Date.now() + WARMUP_MS;
  let warm = false;
  return async (path: string, at: Range['start']): Promise<Location[]> => {
    for (;;) {
      const r = await lsp(api, 'lsp_references', { path, line: at.line, character: at.character, includeDeclaration: false }, ctx);
      const refs = Array.isArray(r) ? (r as Location[]) : [];
      warm ||= refs.some((l) => !l.uri.endsWith(`/${path}`)) || Date.now() > until;
      if (warm) return refs;
      await sleep(250, undefined, { signal: ctx.abortSignal });
    }
  };
}

// GH-3: who depends on each changed symbol, from the language servers; empty when the session has no LSP tools.
export async function blastRadius(api: ToolExecutionApi, repoDir: string, diff: string, ctx: Context): Promise<string> {
  const names = new Set((await api.agent(ctx)).callable.map((t) => t.name));
  if (!names.has('lsp_symbols') || !names.has('lsp_references')) return '';
  const references = referenceFinder(api, ctx);
  const root = `${api.env!.cwd}/${repoDir}`;
  const lines: string[] = [];
  let symbols = 0;
  for (const file of changedLines(diff).slice(0, MAX_FILES)) {
    const path = `${repoDir}/${file.path}`;
    const text = await api.env!.readTextFile(path, ctx);
    for (const sym of touchedSymbols(await lsp(api, 'lsp_symbols', { path }, ctx), file.lines, text.ok ? text.value : '')) {
      if (++symbols > MAX_SYMBOLS) break;
      lines.push(describe(sym, file.path, await references(path, sym.at), root));
    }
  }
  return lines.length ? `Blast radius of changed symbols:\n${lines.join('\n')}` : '';
}
