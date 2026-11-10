// Spike: chunk a recorded PR's diff into functions, test cases, doc sections and spec files, ask Clef the lint questions
// of each chunk, and write the answers for comparison with the recorded review. No model call happens in plan mode.
import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, normalize } from 'node:path';
import ts from 'typescript';
import { changedRanges } from '../sloplint.ts';
import { DOCS_WITH_CODE, instructions, RULES, type Lint, type Rule } from './clef-lint-rules.ts';

type Range = [number, number];
export type Unit = { start: number; end: number; enclosing: string[]; test?: boolean };
export type Chunk = { id: string; lints: Lint[]; file: string; start: number; end: number; state: Record<string, unknown> };
type Finding = { rule: string; file: string; line: number; note: string };

// Clef's cap is 8,192 input tokens and code packs about 3 bytes a token (packages/judge/src/resolve.ts).
const CAP_BYTES = 7600 * 3;
const CLEF = process.env.CLEF_URL ?? 'http://jason-server.hs.internal:11436/upstream/clef/v1/systemone';
const ROUTE = /['"`]\/(v\d+|api)\/|\bmethod\s*===\s*['"](GET|POST|PATCH|PUT|DELETE)|\breq(uest)?\.method\b/;
const TEST_CALL = /^(test|it|describe|suite|t\.test)(\.(only|skip|todo|each))?$/;

const git = (args: string[]) => execFileSync('git', args, { encoding: 'utf8', maxBuffer: 1 << 30 });
const overlaps = (u: { start: number; end: number }, rs: Range[]) => rs.some(([a, b]) => u.start <= b && u.end >= a);

export function kindOf(path: string): 'tests' | 'code' | 'docs' | 'spec' | undefined {
  if (/fixtures-specs\/|\.schema\.json$|openapi[^/]*\.(json|ya?ml)$/.test(path)) return 'spec';
  if (/\.md$/.test(path) && !/^(v1\/)?agents\//.test(path)) return 'docs';
  if (!/\.tsx?$/.test(path) || /\.d\.ts$/.test(path)) return undefined;
  return /\.test\.tsx?$|\/test\//.test(path) ? 'tests' : 'code';
}

const calleeOf = (s: ts.Statement) => {
  const e = ts.isExpressionStatement(s) ? s.expression : undefined;
  const call = e && ts.isAwaitExpression(e) ? e.expression : e;
  return call && ts.isCallExpression(call) ? call : undefined;
};

// A node's first line, counting the comments right above it, so comment questions see them.
function span(sf: ts.SourceFile, node: ts.Node): [number, number] {
  const lead = ts.getLeadingCommentRanges(sf.text, node.pos)?.[0]?.pos ?? node.getStart(sf);
  return [sf.getLineAndCharacterOfPosition(lead).line + 1, sf.getLineAndCharacterOfPosition(node.end).line + 1];
}

const MAX_LINES = 80;
const isPart = (n: ts.Node) => (ts.isStatement(n) && !ts.isBlock(n)) || ts.isClassElement(n) || ts.isObjectLiteralElement(n);
const parts = (n: ts.Node): ts.Node[] => n.getChildren().flatMap((c) => (isPart(c) ? [c] : parts(c)));
const headerOf = (sf: ts.SourceFile, n: ts.Node) => sf.text.slice(n.getStart(sf)).split('\n')[0].trim().slice(0, 200);

// A unit over MAX_LINES splits into its changed nested statements, methods and properties, keeping its first line as context.
// shortcut: a changed line in a big unit's own header, outside every part, is dropped; rare in practice.
function refine(sf: ts.SourceFile, n: ts.Node, u: Unit, rs: Range[]): Unit[] {
  if (u.end - u.start < MAX_LINES) return [u];
  const kids = parts(n).map((k) => [k, span(sf, k)] as const).filter(([, [start, end]]) => overlaps({ start, end }, rs));
  if (!kids.length) return [u];
  const enclosing = [...u.enclosing, headerOf(sf, n)];
  return kids.flatMap(([k, [start, end]]) => refine(sf, k, { start, end, enclosing }, rs));
}

// Adjacent small units that are not test cases go in one chunk, so a run of one-line declarations is one request.
function merge(units: Unit[]): Unit[] {
  const out: Unit[] = [];
  for (const u of units) {
    const last = out.at(-1);
    const near = last && !last.test && !u.test && u.start - last.end <= 3 && u.end - last.start < MAX_LINES && last.enclosing.join() === u.enclosing.join();
    if (near) last.end = u.end;
    else out.push({ ...u });
  }
  return out;
}

const describeBody = (sf: ts.SourceFile, s: ts.Statement) => {
  const call = calleeOf(s);
  const fn = call?.arguments.at(-1);
  const named = call && /^(describe|suite)(\.\w+)?$/.test(call.expression.getText(sf));
  return named && fn && (ts.isArrowFunction(fn) || ts.isFunctionExpression(fn)) && ts.isBlock(fn.body) ? { title: call.arguments[0]?.getText(sf) ?? '', body: fn.body.statements } : undefined;
};
const isCase = (sf: ts.SourceFile, s: ts.Node) => ts.isStatement(s) && !!calleeOf(s) && TEST_CALL.test(calleeOf(s)!.expression.getText(sf));

/** Changed top-level declarations, class members and test cases (inside describes) of a TypeScript file, split when big. */
export function tsUnits(path: string, text: string, test: boolean, rs: Range[]): Unit[] {
  const sf = ts.createSourceFile(path, text, ts.ScriptTarget.Latest, true);
  const out: Unit[] = [];
  const add = (n: ts.Node, enclosing: string[]) => {
    const [start, end] = span(sf, n);
    const u = { start, end, enclosing, test: test && isCase(sf, n) };
    if (overlaps(u, rs)) out.push(...(u.test ? [u] : refine(sf, n, u, rs)));
  };
  const visit = (stmts: readonly ts.Statement[], enclosing: string[]) => {
    for (const s of stmts) {
      const d = test ? describeBody(sf, s) : undefined;
      if (ts.isImportDeclaration(s)) continue;
      if (d) visit(d.body, [...enclosing, d.title]);
      else if (ts.isClassDeclaration(s)) s.members.forEach((m) => add(m, [...enclosing, headerOf(sf, s)]));
      else add(s, enclosing);
    }
  };
  visit(sf.statements, []);
  return merge(out);
}

/** One unit per heading's section; the text above the first heading is a section of its own. */
export function mdUnits(text: string): Unit[] {
  const lines = text.split('\n');
  const out: Unit[] = [];
  const path: string[] = [];
  let fence = false;
  lines.forEach((l, i) => {
    if (/^\s*(```|~~~)/.test(l)) fence = !fence;
    const h = !fence && /^(#{1,6})\s+(.*)/.exec(l);
    if (!h && i > 0) return;
    if (out.length) out.at(-1)!.end = i;
    if (h) path.splice(h[1].length - 1, Infinity, h[2]);
    out.push({ start: i + 1, end: lines.length, enclosing: [...path] });
  });
  return out;
}

const numbered = (lines: string[], u: { start: number; end: number }, rs: Range[]) =>
  lines.slice(u.start - 1, u.end).map((l, i) => `${overlaps({ start: u.start + i, end: u.start + i }, rs) ? '+' : ' '}${u.start + i}| ${l}`).join('\n');

const imports = (text: string) => text.split('\n').filter((l) => /^import\s/.test(l)).join('\n').slice(0, 1500);

// A unit too big for one request goes in line windows; each window keeps the file context.
function windows(u: Unit, lines: string[], rs: Range[], context: Record<string, unknown>): Array<{ start: number; end: number; body: string }> {
  const whole = numbered(lines, u, rs);
  const parts = Math.ceil((whole.length + JSON.stringify(context).length + 3000) / CAP_BYTES);
  const step = Math.ceil((u.end - u.start + 1) / parts);
  const out = [];
  for (let s = u.start; s <= u.end; s += step) {
    const w = { start: s, end: Math.min(u.end, s + step - 1) };
    if (overlaps(w, rs)) out.push({ ...w, body: numbered(lines, w, rs) });
  }
  return out;
}

function lintsFor(kind: string, body: string): Lint[] {
  if (kind === 'spec') return ['contracts'];
  if (kind === 'docs') return ['docs'];
  if (kind === 'tests') return ['tests'];
  return ROUTE.test(body) ? ['code', 'contracts'] : ['code'];
}

/** The chunks of one file at the head: changed units only, each with its file's imports and enclosing names. */
export function chunksOf(file: string, text: string, rs: Range[]): Chunk[] {
  const kind = kindOf(file);
  if (!kind) return [];
  const lines = text.split('\n');
  const units = kind === 'docs' ? mdUnits(text) : kind === 'spec' ? [{ start: 1, end: lines.length, enclosing: [] }] : tsUnits(file, text, kind === 'tests', rs);
  return units.filter((u) => overlaps(u, rs)).flatMap((u) => {
    const context = { file, imports: kind === 'docs' ? undefined : imports(text), enclosing: u.enclosing.join(' > ') || undefined };
    return windows(u, lines, rs, context).map((w) => ({ id: `${file}:${w.start}-${w.end}`, lints: lintsFor(kind, w.body), file, start: w.start, end: w.end, state: { ...context, lines: w.body } }));
  });
}

type Case = { case: string; head: string; base: string; title: string; body: string };

function deleted(c: Case): string[] {
  return git(['diff', '--name-status', '-M', c.base, c.head]).split('\n').filter((l) => /^[DR]/.test(l)).map((l) => l.split('\t')[1]);
}

// Paths the PR removed that the head still names; a lockfile or the changelog doesn't count.
function dangling(c: Case): Finding[] {
  const gone = deleted(c);
  const files = new Set(git(['ls-tree', '-r', '--name-only', c.head]).split('\n'));
  const dirs = [...new Set(gone.map((p) => dirname(p)))].filter((d) => ![...files].some((f) => f.startsWith(`${d}/`)));
  const found = new Map<string, Finding>();
  for (const p of [...dirs, ...gone.filter((p) => !files.has(p))]) {
    // git grep exits 1 when nothing matches.
    const hits = (() => { try { return git(['grep', '-n', '-F', '-e', p.replace(/^v1\//, ''), c.head, '--', '.', ':!*package-lock.json']); } catch { return ''; } })();
    for (const h of hits.split('\n').filter(Boolean)) {
      const [, file, line] = h.split(':');
      found.set(`${file}:${line}`, { rule: 'dangling-ref', file, line: Number(line), note: `names removed path ${p}` });
    }
  }
  return [...found.values()];
}

function brokenLinks(c: Case, chunks: Chunk[], files: Set<string>): Finding[] {
  return chunks.filter((k) => k.lints.includes('docs')).flatMap((k) => [...String(k.state.lines).matchAll(/^\+(\d+)\|.*?\]\(([^)#\s]+)/gm)].flatMap(([, line, href]) => {
    if (/^[a-z]+:/i.test(href)) return [];
    const target = normalize(join(dirname(k.file), href)).replace(/\/$/, '');
    return files.has(target) || [...files].some((f) => f.startsWith(`${target}/`)) ? [] : [{ rule: 'broken-link', file: k.file, line: Number(line), note: `links ${href}` }];
  }));
}

function untested(c: Case): Finding[] {
  const pkg = (p: string) => /^((v1\/)?(packages\/[^/]+|tools))\//.exec(p)?.[1];
  const rows = git(['diff', '--numstat', c.base, c.head]).split('\n').filter(Boolean).map((l) => l.split('\t'));
  const src = new Map<string, number>();
  const tested = new Set<string>();
  for (const [a, d, p] of rows) {
    const k = kindOf(p), g = pkg(p);
    if (!g) continue;
    if (k === 'tests') tested.add(g);
    if (k === 'code') src.set(g, (src.get(g) ?? 0) + Number(a) + Number(d));
  }
  return [...src].filter(([g, n]) => n > 10 && !tested.has(g)).map(([g, n]) => ({ rule: 'logic-without-tests', file: g, line: 0, note: `${n} src lines changed, no test changed` }));
}

export function plan(c: Case) {
  const ranges = changedRanges(git(['diff', '-U0', '--find-renames', '-l0', c.base, c.head]));
  const files = new Set(git(['ls-tree', '-r', '--name-only', c.head]).split('\n'));
  const chunks = [...ranges].flatMap(([file, rs]) => (kindOf(file) ? chunksOf(file, git(['show', `${c.head}:${file}`]), rs) : []));
  const docsTouched = [...ranges.keys()].some((f) => kindOf(f) === 'docs');
  if (!docsTouched && [...ranges.keys()].some((f) => kindOf(f) === 'code')) {
    chunks.push({ id: 'pr:docs-with-code', lints: [], file: '(pull request)', start: 0, end: 0, state: { title: c.title, description: c.body.slice(0, 6000), changed: [...ranges.keys()].join('\n') } });
  }
  const skipped = [...ranges].filter(([f]) => !kindOf(f)).map(([f, rs]) => [f, rs.reduce((s, [a, b]) => s + b - a + 1, 0)]);
  return { chunks, skipped, deterministic: [...dangling(c), ...brokenLinks(c, chunks, files), ...untested(c)] };
}

const ruleSet = (k: Chunk, lint: Lint | 'pr') => (lint === 'pr' ? [DOCS_WITH_CODE] : RULES[lint]);
export const request = (k: Chunk, lint: Lint | 'pr') => ({
  model: 'clef',
  state: k.state,
  questions: Object.fromEntries(ruleSet(k, lint).map((r: Rule) => [r.id, { type: 'noul', instructions: lint === 'pr' ? r.ask : instructions(r), criteria: { true: 'Yes, this holds.', false: 'No, it does not hold.' } }])),
});

async function ask(body: unknown): Promise<{ answers: Record<string, number>; ms: number; usage?: unknown }> {
  const t = performance.now();
  const r = await fetch(CLEF, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) });
  if (!r.ok) throw new Error(`clef ${r.status}: ${await r.text()}`);
  const j = (await r.json()) as { answers: Record<string, { noul: number }>; usage?: unknown };
  return { answers: Object.fromEntries(Object.entries(j.answers).map(([k, v]) => [k, v.noul])), ms: Math.round(performance.now() - t), usage: j.usage };
}

function loadCases(lab: string): Case[] {
  const seen = new Set<string>();
  return JSON.parse(readFileSync(join(lab, 'cases.json'), 'utf8')).flatMap((x: { case: string }) => {
    const pr = JSON.parse(readFileSync(join(lab, 'prs', `${x.case}.json`), 'utf8')).pr;
    if (seen.has(pr.headSha)) return [];
    seen.add(pr.headSha);
    return [{ case: x.case, head: pr.headSha, base: git(['merge-base', 'origin/main', pr.headSha]).trim(), title: pr.title, body: pr.body }];
  });
}

// The judge acts on Clef at 0.8 either way (packages/judge/src/resolve.ts); between, it would escalate to the 27B.
const ACT = 0.8;
type Answer = { answers: Record<string, number>; ms: number };
type Flag = { case: string; lint: string; rule: string; file: string; start: number; end: number; p: number; cited: boolean };

// file:line citations in a review's Findings and Notes (Verified cites what was read), without the v1/ prefix older heads carried.
const section = (review: string, name: string) => [...review.matchAll(/(?:^|\n)## (\w+)\n([\s\S]*?)(?=\n## |$)/g)].find((m) => m[1] === name)?.[2] ?? '';
const citations = (review: string) => [...`${section(review, 'Findings')}\n${section(review, 'Notes')}`.matchAll(/([\w./-]+\.(?:tsx?|md)):(\d+)/g)].map(([, f, l]) => [f.replace(/^v1\//, ''), Number(l)] as const);

function flags(name: string, rec: { chunks: Chunk[]; answers: Record<string, Answer> }, cited: ReadonlyArray<readonly [string, number]>) {
  const out: Flag[] = [];
  let unsure = 0;
  for (const [key, a] of Object.entries(rec.answers)) {
    const [id, lint] = key.split('#');
    const k = rec.chunks.find((c) => c.id === id)!;
    for (const [rule, p] of Object.entries(a.answers)) {
      if (p > 1 - ACT && p < ACT) unsure++;
      const hit = cited.some(([f, l]) => k.file.replace(/^v1\//, '') === f && l >= k.start - 3 && l <= k.end + 3);
      if (p >= ACT) out.push({ case: name, lint, rule, file: k.file, start: k.start, end: k.end, p: Math.round(p * 100) / 100, cited: hit });
    }
  }
  return { out, unsure };
}

/** Per recorded review: Clef calls and time, flags by lint, would-escalate answers, and flags in a chunk the review cites. */
function report(lab: string, out: string) {
  const reviews = JSON.parse(readFileSync(join(lab, 'cases.json'), 'utf8')) as Array<{ case: string; content: string }>;
  const all: Flag[] = [];
  const rows = reviews.map((r) => {
    const head = JSON.parse(readFileSync(join(lab, 'prs', `${r.case}.json`), 'utf8')).pr.headSha;
    const recFile = readdirSync(out).find((f) => JSON.parse(readFileSync(join(out, f), 'utf8')).case.head === head)!;
    const rec = JSON.parse(readFileSync(join(out, recFile), 'utf8'));
    const { out: fl, unsure } = flags(r.case, rec, citations(r.content));
    all.push(...fl);
    const answers = Object.values(rec.answers as Record<string, Answer>);
    const by = (l: string) => fl.filter((f) => f.lint === l).length;
    return `| ${r.case} | ${rec.chunks.length} | ${answers.length} | ${Math.round(answers.reduce((s, a) => s + a.ms, 0) / 1000)} | ${by('code')} | ${by('tests')} | ${by('docs')} | ${by('contracts')} | ${by('pr')} | ${rec.deterministic.length} | ${unsure} | ${fl.filter((f) => f.cited).length} |`;
  });
  writeFileSync(join(out, '..', 'flags.json'), JSON.stringify(all, null, 1));
  console.log(['| case | chunks | Clef calls | Clef s | code | tests | docs | contracts | docs-with-code | deterministic | would escalate | in a cited chunk |', '|---|---|---|---|---|---|---|---|---|---|---|---|', ...rows].join('\n'));
  const rules = new Map<string, number>();
  for (const f of all) rules.set(`${f.lint}/${f.rule}`, (rules.get(`${f.lint}/${f.rule}`) ?? 0) + 1);
  console.log([...rules].sort((a, b) => b[1] - a[1]).map(([r, n]) => `${r} ${n}`).join('\n'));
}

async function main([mode, lab, out]: string[]) {
  if (mode === 'report') return report(lab, out);
  if (!['plan', 'run'].includes(mode) || !lab || !out) throw new Error('usage: node tools/bench/clef-lint.ts <plan|run|report> <reviewer-replay dir> <out dir>');
  mkdirSync(out, { recursive: true });
  for (const c of loadCases(lab)) {
    const p = plan(c);
    const calls = p.chunks.flatMap((k) => (k.lints.length ? k.lints : ['pr' as const]).map((lint) => ({ k, lint })));
    const bytes = calls.map(({ k, lint }) => JSON.stringify(request(k, lint)).length);
    console.log(`${c.case} chunks=${p.chunks.length} calls=${calls.length} maxTokens~${Math.round(Math.max(0, ...bytes) / 3)} sumTokens~${Math.round(bytes.reduce((a, b) => a + b, 0) / 3)} deterministic=${p.deterministic.length} skipped=${p.skipped.length}`);
    const file = join(out, `${c.case}.json`);
    const prior = existsSync(file) ? JSON.parse(readFileSync(file, 'utf8')).answers ?? {} : {};
    const answers: Record<string, unknown> = { ...prior };
    for (const { k, lint } of mode === 'run' ? calls : []) {
      const key = `${k.id}#${lint}`;
      answers[key] ??= await ask(request(k, lint));
      writeFileSync(file, JSON.stringify({ case: c, ...p, answers }, null, 1));
    }
    writeFileSync(file, JSON.stringify({ case: c, ...p, answers }, null, 1));
  }
}

if (import.meta.main) await main(process.argv.slice(2));
