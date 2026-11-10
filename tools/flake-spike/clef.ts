// Detector B: one Clef request of yes/no questions per test, its state only the test, its file helpers and its imports.
// Usage: QUACK_CLEF_API_KEY=none node tools/flake-spike/clef.ts [prs|known] [--dry] [--deployment path]
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';
import ts from 'typescript';
import type { ClassifierModel, ClassifierQuestion } from '@earendil-works/pi-ai';
import { buildModels, parseDeployment } from '@quack/session';
import type { Case } from './cases.ts';
import { sample } from './mutate.ts';
import { ROOT } from './run.ts';
import { all, caseSet, parse, text } from './static.ts';

const CAP = 8192;
const BYTES_PER_TOKEN = 3; // what packages/judge/src/resolve.ts budgets for code
const ACT = 0.8;

/** The questions, each tied to the rule in "Software Engineering at Google" it asks about. */
export const QUESTIONS: Record<string, { category: 'flaky' | 'taut'; q: string }> = {
  time: { category: 'flaky', q: 'Does the test outcome depend on real time: a real sleep or timer, a wall-clock read, or work that must finish within a deadline, rather than a fake or injected clock? (SWE at Google ch11: small tests make no sleeps; ch13: clock time)' },
  port: { category: 'flaky', q: 'Does the test, or the code it runs, reserve a network port, release it, and later have a server bind that number, so another process could take it in between? (ch13 nondeterminism)' },
  shared: { category: 'flaky', q: 'Does the test write to a fixed path outside a fresh temp dir, set an environment variable without restoring it, or read or change a module-level variable that outlives the test? (ch11 hermetic tests)' },
  external: { category: 'flaky', q: 'Does the test reach a real network service or external system outside its own process and the servers it starts? (ch11 hermetic; ch13 non-hermetic services)' },
  wait: { category: 'flaky', q: 'Does the test, or the code it runs, wait for a child process or server to become ready in a way that would not notice the child exiting? (ch11 no blocking calls)' },
  order: { category: 'flaky', q: 'Does an assertion depend on the order concurrent work finishes in, or on random values? (ch13 thread scheduling)' },
  broken: { category: 'taut', q: 'Could this test still pass if the code under test were broken, for example returning a wrong or empty result? (ch11 hollow coverage)' },
  mocks: { category: 'taut', q: 'Does the test assert only on mocks, on calls made to mocks, or on values the test itself set, rather than on what the real code returned or did? (ch12 test state, not interactions; ch13 mocks last)' },
  logic: { category: 'taut', q: 'Does the test compute its expected value with logic such as loops, conditionals or a call to the code under test, instead of stating it? (ch12 no logic in tests)' },
};

const CRITERIA = { true: 'Yes, this holds.', false: 'No, it does not hold.' };
const questions = (): Record<string, ClassifierQuestion> => Object.fromEntries(Object.entries(QUESTIONS).map(([k, v]) => [k, { type: 'bool', instructions: v.q, criteria: CRITERIA }]));
const isTest = (n: ts.Node): n is ts.CallExpression => ts.isCallExpression(n) && /^(test|it)$/.test(text(n.expression)) && !!n.arguments[0] && ts.isStringLiteralLike(n.arguments[0]);

/** The test's own text and the file helpers it names; the subjects' functions it names, then the rest of each subject, to fit. */
export function state(c: Case): { test: string; helpers: string; code: string } {
  const sf = parse(c.file, c.rev);
  const own = c.name ? all([sf], isTest).find((t) => (t.arguments[0] as ts.StringLiteral).text === c.name) : undefined;
  const testText = own ? own.getText() : sf.text;
  const named = (s: ts.Statement) => (ts.isFunctionDeclaration(s) && s.name ? [s.name.text] : ts.isVariableStatement(s) ? s.declarationList.declarations.map((d) => d.name.getText()) : []);
  const used = (s: ts.Statement) => named(s).some((n) => new RegExp(`\\b${n}\\b`).test(testText));
  const helpers = own ? sf.statements.filter((s) => !all([s], isTest).length && (ts.isImportDeclaration(s) || used(s))).map((s) => s.getText()).join('\n') : '';
  const ordered = (s: ts.SourceFile) => [...s.statements.filter(used), ...s.statements.filter((x) => !used(x) && !ts.isImportDeclaration(x))].map((x) => x.getText()).join('\n');
  const code = c.subjects.map((p) => parse(p, c.rev)).map((s) => `// ${s.fileName}\n${ordered(s)}`).join('\n');
  return fit({ test: testText, helpers, code });
}

// Trim helpers, then code, then the test itself, until the request is under Clef's cap.
function fit(s: { test: string; helpers: string; code: string }) {
  const budget = CAP * BYTES_PER_TOKEN - JSON.stringify(questions()).length - 200;
  const over = () => JSON.stringify(s).length - budget;
  for (const k of ['code', 'helpers', 'test'] as const) if (over() > 0) s[k] = `${s[k].slice(0, Math.max(0, s[k].length - over()))}\n// ...cut to fit`;
  return s;
}

function clefModel(deploymentPath: string) {
  const m = buildModels(parseDeployment(JSON.parse(readFileSync(deploymentPath, 'utf8'))));
  return { m, clef: m.getAllModels('clef').find((x) => x.type === 'classifier') as ClassifierModel<never> };
}

export type Answer = { p?: number; verdict: 'yes' | 'no' | 'unsure' | 'error' };
export type ClefResult = { id: string; answers: Record<string, Answer>; tokens: number; ms: number };

async function judge(mm: ReturnType<typeof clefModel>, c: Case): Promise<ClefResult> {
  const started = Date.now();
  const s = state(c);
  const tokens = Math.round(JSON.stringify({ state: s, questions: questions() }).length / BYTES_PER_TOKEN);
  const r = await mm.m.classify(mm.clef, { state: s, questions: questions() }, { maxRetries: 0 }).catch(() => undefined);
  const answers = Object.fromEntries(Object.keys(QUESTIONS).map((k): [string, Answer] => {
    const p = r?.stopReason === 'stop' ? (r.answers[k] as { probability?: number } | undefined)?.probability : undefined;
    if (p === undefined) return [k, { verdict: 'error' }];
    return [k, { p, verdict: p >= ACT ? 'yes' : p <= 1 - ACT ? 'no' : 'unsure' }];
  }));
  return { id: c.id, answers, tokens, ms: Date.now() - started };
}

const SEED = 'flake-spike-2026-11-10';
const read = (f: string) => JSON.parse(readFileSync(join(ROOT, 'tools/flake-spike/results', f), 'utf8'));

/** PR tests any model-free detector flagged, plus 150 unflagged ones drawn with SEED. */
export function prSelection(cases: Array<Case & { head?: string }>): { flagged: string[]; sampled: string[] } {
  const flagged = new Set<string>(read('static-prs.json').results.filter((r: { hits: unknown[] }) => r.hits.length).map((r: { id: string }) => r.id));
  for (const r of read('mutate-prs.json').results) if (r.status === 'ok' && r.mutants >= 3 && r.killed === 0) flagged.add(r.id);
  for (const r of read('repeat-prs.json').results.filter((x: { failed: number }) => x.failed)) {
    const inFile = cases.filter((c) => c.head === r.id);
    const named = inFile.filter((c) => r.names.some((n: string) => n.includes(c.name!)));
    for (const c of named.length ? named : inFile) flagged.add(c.id);
  }
  const rest = cases.filter((c) => !flagged.has(c.id)).map((c) => c.id);
  return { flagged: [...flagged], sampled: sample(rest, SEED, 150) };
}

if (import.meta.main) {
  const args = process.argv.slice(2);
  const prs = args.includes('prs');
  const { cases, out } = caseSet(args.find((a) => a === 'prs' || a === 'known'));
  const pick = prs ? prSelection(cases) : undefined;
  const list = pick ? cases.filter((c) => pick.flagged.includes(c.id) || pick.sampled.includes(c.id)) : cases;
  const sizes = list.map((c) => Math.round(JSON.stringify({ state: state(c), questions: questions() }).length / BYTES_PER_TOKEN)).sort((a, b) => a - b);
  console.log(`${list.length} requests (${pick ? `${pick.flagged.length} flagged, ${pick.sampled.length} sampled, seed ${SEED}` : out || 'corpus'}); tokens median ${sizes[sizes.length >> 1]}, max ${sizes.at(-1)}, total ${sizes.reduce((a, b) => a + b, 0)}`);
  if (pick) writeFileSync(join(ROOT, 'tools/flake-spike/results/clef-selection.json'), `${JSON.stringify({ seed: SEED, ...pick }, null, 1)}\n`);
  if (args.includes('--dry')) process.exit(0);
  const at = args.indexOf('--deployment');
  const mm = clefModel(at >= 0 ? args[at + 1] : join(homedir(), 'quack-lab/reviewer-replay/deployment.json'));
  const results: ClefResult[] = [];
  const started = Date.now();
  // Clef serves one request at a time; going serially keeps prod's queue short.
  for (const c of list) {
    const r = await judge(mm, c);
    results.push(r);
    console.log(`${results.length}/${list.length} ${r.id} ${r.ms}ms ${Object.entries(r.answers).map(([k, a]) => `${k}=${a.verdict}`).join(' ')}`);
  }
  mkdirSync(join(ROOT, 'tools/flake-spike/results'), { recursive: true });
  writeFileSync(join(ROOT, `tools/flake-spike/results/clef${out}.json`), `${JSON.stringify({ ms: Date.now() - started, results }, null, 1)}\n`);
}
