import { execFileSync, spawnSync } from 'node:child_process';
import { randomInt } from 'node:crypto';
import { existsSync, mkdirSync, mkdtempSync, readdirSync, rmSync, symlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import type { MutableModels } from '@earendil-works/pi-ai';
import { obj } from '@earendil-works/pi-schema';
import type { Context } from '@earendil-works/chord/context';
import { defineTool, type ToolExecutionApi } from '@earendil-works/pi-durable';
import { rejudge, type DecisionModelRef, type Declaration, type Inputs, type JudgeOptions, type ModelRef, type Rubric } from '@quack/judge';
import { extensionList, loadManifests } from '@quack/runtime/catalog';
import type { EvalAdapter, RecordedSession } from '@quack/sdk';
import { adaptersOf } from './adapters.ts';
import { driveTool } from './driver.ts';
import { asJudged, readSession } from './record.ts';

/** A recorded artifact to judge: from a session directory, or with its content and inputs given inline; `units` keeps only those items. */
export type Case = { case: string; kind: string; checks?: string[]; units?: string[]; session?: string; version?: number; content?: string; inputs?: Inputs };
/** An arm judges with its adapters' declarations and inputs, its judge engine (this tree's when unset) and, when it has one, a per-unit decision model before the judge model. */
export type Arm = { label: string; adapters: EvalAdapter[]; rejudge?: typeof rejudge; decisionModel?: DecisionModelRef; /** Item reuse on: each repeat judges a recorded session's cases in version order against one store of their own. */ reuse?: boolean };

/** A tar of `packages/` as it stood at ref in the repo at `top`; a ref from before the repo-root layout keeps it under v1/. */
export function packagesTar(top: string, ref: string): Buffer {
  const base = spawnSync('git', ['cat-file', '-e', `${ref}:packages`], { cwd: top }).status === 0 ? '' : 'v1';
  return execFileSync('git', ['archive', '--format=tar', `${ref}:${base}`, 'packages'], { cwd: top, maxBuffer: 1 << 30 });
}

/** An arm's adapters and judge engine as they stood at ref (`.` is this tree). */
export async function armAt(ref: string): Promise<Pick<Arm, 'adapters' | 'rejudge'>> {
  if (ref === '.') return { adapters: adaptersOf(await loadManifests()) };
  const dir = mkdtempSync(join(tmpdir(), 'quack-arm-'));
  // The arm reads its tree lazily (the contracts lint finds @types/node under it), so it must outlive this call.
  process.once('exit', () => rmSync(dir, { recursive: true, force: true }));
  const top = execFileSync('git', ['rev-parse', '--show-toplevel'], { encoding: 'utf8' }).trim();
  execFileSync('tar', ['-x', '-C', dir], { input: packagesTar(top, ref) });
  return loadArm(dir);
}

/** Links the `packages/` tree in dir so its @quack/* imports resolve inside dir; third-party modules stay this tree's, shared with the harness. */
export function linkTree(dir: string): void {
  const modules = fileURLToPath(new URL('../../../node_modules', import.meta.url));
  mkdirSync(join(dir, 'node_modules/@quack'), { recursive: true });
  for (const m of readdirSync(modules).filter((m) => m !== '@quack')) symlinkSync(join(modules, m), join(dir, 'node_modules', m));
  for (const p of readdirSync(join(dir, 'packages'))) {
    symlinkSync(join(dir, 'packages', p), join(dir, 'node_modules/@quack', p));
    // npm nests a dependency under its workspace when the root holds a conflicting version.
    const nested = join(modules, '../packages', p, 'node_modules');
    if (existsSync(nested)) symlinkSync(nested, join(dir, 'packages', p, 'node_modules'));
  }
}

/** The adapters and engine of the `packages/` tree in dir. */
export async function loadArm(dir: string): Promise<Pick<Arm, 'adapters' | 'rejudge'>> {
  linkTree(dir);
  const at = (pkg: string) => pathToFileURL(join(dir, 'packages', pkg, 'src/index.ts')).href;
  const specs = extensionList().filter((s) => s.startsWith('@quack/')).map((s) => at(s.slice(7)));
  const engine = (await import(at('judge'))) as typeof import('@quack/judge');
  return { adapters: adaptersOf(await loadManifests(specs)), rejudge: engine.rejudge };
}
/** One check of one job: `pos` is the arm's place in its case's order that repeat (1 first); `clef`, `esc` and `reused` count settled items; `ms` and `requests` (by model id) are the job's.
 * `route` says whether the arm asked the decision model, the check is declared off it, or the arm has none; `rubric`, `criterion` and `route` are absent in older rows files. */
export type Row = { arm: string; rep: number; pos: number; case: string; rubric?: string; criterion?: string; check: string; route?: Route; met: number | null; clef: number; esc: number; reused: number; ms: number; requests: Record<string, number>; notes: string[] };
/** A case an arm could not judge, with what its recording lacks. */
export type Declined = { arm: string; case: string; reason: string };
export type Replayed = { rows: Row[]; declined: Declined[]; seed?: number };
/** `seed` orders the arms within each case and repeat (random when unset); `onRows` gets each job's rows as it finishes. */
export type Options = { cases: Case[]; arms: Arm[]; repeat: number; concurrency: number; models: MutableModels; judgeModel: ModelRef; seed?: number; onRows?: (rows: Row[]) => void };

type Route = 'asked' | 'declared' | 'not-asked';
const routeOf = (j: Job, check: string): Route => (j.judged.declaration.checks[check]?.decisionModel === false ? 'declared' : j.decisionModel ? 'asked' : 'not-asked');
// Items no code and no decision model settled were escalated; an older arm names them judge-model or agent-model, a newer one by modelId.
export const escalated = (resolvers: Record<string, number>, decisionModel?: string) => Object.entries(resolvers).reduce((n, [r, c]) => (r === 'code' || r === decisionModel ? n : n + c), 0);
/** What a job is judged on: the rubric's declaration and inputs. */
type Judged = { declaration: Declaration; inputs: Inputs };
type Prepared = Omit<Job, 'rep' | 'pos'> & { found: Set<string> };
type Job = { arm: string; rejudge: typeof rejudge; decisionModel?: DecisionModelRef; reuse?: boolean; rep: number; pos: number; c: Case; rubric: Rubric; content: string; judged: Judged };

/** The rubric cut to the watched checks (by id or criterion.id), skipping names this arm's rubric lacks. */
function watch(rubric: Rubric, names: string[] | undefined): { rubric: Rubric; found: Set<string> } {
  if (!names?.length) return { rubric, found: new Set() };
  const hit = (cid: string, id: string) => names.includes(id) || names.includes(`${cid}.${id}`);
  const criteria = rubric.criteria.map((c) => ({ ...c, checks: c.checks.filter((k) => hit(c.id, k.id)) })).filter((c) => c.checks.length);
  const found = new Set(criteria.flatMap((c) => c.checks.flatMap((k) => [k.id, `${c.id}.${k.id}`])));
  return { rubric: { ...rubric, criteria }, found };
}

// An adapter that throws is declining the case for a reason (what the recording lacks); a case file without content is an error.
async function prepare(arm: Arm, c: Case): Promise<Prepared | Declined> {
  // Adapters find the judged version by content, so they see the session as it was judged: a later version with the same content is not it.
  const record: RecordedSession = c.session ? asJudged(await readSession(c.session), c.kind, c.version) : { docs: {}, artifacts: {} };
  const versions = record.artifacts[c.kind] ?? [];
  const content = c.content ?? (c.version ? versions.find((v) => v.version === c.version) : versions.at(-1))?.content;
  if (content === undefined) throw new Error(`${c.case}: no ${c.kind} content in the case or its session`);
  const reasons = new Set<string>();
  for (const a of arm.adapters.filter((x) => x.kind === c.kind && x.judge)) {
    const judged = await judgedBy(a, c, record, content).catch((err: Error) => void reasons.add(err.message));
    if (!judged) continue;
    const { rubric, found } = watch(a.judge!.rubric as Rubric, c.checks);
    return { arm: arm.label, rejudge: arm.rejudge ?? rejudge, decisionModel: arm.decisionModel, reuse: arm.reuse, c, content, rubric, found, judged: { ...judged, declaration: declared(judged.declaration, rubric, c.units) } };
  }
  return { arm: arm.label, case: c.case, reason: [...reasons].join('; ') || `no declaration serves ${c.kind} with these inputs` };
}

async function judgedBy(a: EvalAdapter, c: Case, record: RecordedSession, content: string): Promise<Judged> {
  const j = a.judge!;
  return { declaration: j.declaration as Declaration, inputs: c.inputs ?? (await j.inputs(record, content)) };
}

/** The declaration cut to the watched rubric's checks, as the rubric is; `units` (an item's id, or its unit's "scope id") keeps only those items of their scopes. */
function declared(d: Declaration, rubric: Rubric, units?: string[]): Declaration {
  const ids = new Set(rubric.criteria.flatMap((c) => c.checks.map((k) => k.id)));
  const checks = Object.fromEntries(Object.entries(d.checks).filter(([id]) => ids.has(id)));
  if (!units) return { ...d, checks };
  const watched = new Set(Object.values(checks).map((k) => k.scope));
  const only = (scope: string) => units.map((u) => (u.startsWith(`${scope} `) ? u.slice(scope.length + 1) : u));
  return { scopes: Object.fromEntries(Object.entries(d.scopes).map(([n, s]) => [n, watched.has(n) ? { ...s, split: { ...s.split, names: only(n) } } : s])), checks };
}

/** The api with `key` read as `value`; every other member is the api's own, its methods bound to it. */
const withKey = <T extends object>(target: T, key: PropertyKey, value: unknown): T =>
  new Proxy(target, { get: (t, k) => {
    const v: unknown = Reflect.get(t, k);
    return k === key ? value : typeof v === 'function' ? v.bind(t) : v;
  } });

// Counts the job's model requests by model id where the judge sends them, so jobs running at once each count their own.
function counted(api: ToolExecutionApi): { api: ToolExecutionApi; requests: Record<string, number> } {
  const requests: Record<string, number> = {};
  const count = (send: (model: { id: string }, ...rest: unknown[]) => unknown) => (model: { id: string }, ...rest: unknown[]) => ((requests[model.id] = (requests[model.id] ?? 0) + 1), send(model, ...rest));
  const models = new Proxy(api.models, { get: (t, k) => {
    const v = Reflect.get(t, k);
    if (typeof v !== 'function') return v;
    return k === 'classify' || k === 'completeSimple' ? count(v.bind(t)) : v.bind(t);
  } });
  return { api: withKey(api, 'models', models), requests };
}

async function judgeJob(api: ToolExecutionApi, ctx: Context, o: Options, j: Job): Promise<Row[]> {
  const sealed = { kind: j.c.kind, version: j.c.version ?? 1, hash: '', content: j.content, sealedAt: 0, verdict: null };
  const opts: JudgeOptions = { kind: j.c.kind, rubric: j.rubric, maxRevisions: 0, decisionModel: j.decisionModel, judgeModel: o.judgeModel, ...(j.reuse && { reuse: true }), declaration: j.judged.declaration, inputs: async () => j.judged.inputs };
  const { api: on, requests } = counted(api);
  const t = Date.now();
  const v = await j.rejudge(on, opts, sealed, `${j.arm}:${j.rep}:${j.c.case}`, ctx);
  const ms = Date.now() - t;
  // An undecided check is already met null; a decided fold reads as 1 or 0 beside the shares of undeclared rubrics.
  const at = { arm: j.arm, rep: j.rep, pos: j.pos, case: j.c.case, ms, requests };
  return v.checks.map((k) => ({ ...at, rubric: v.rubric, criterion: k.criterion, check: k.id, route: routeOf(j, k.id), met: typeof k.met === 'boolean' ? Number(k.met) : k.met, clef: j.decisionModel ? k.resolvers[j.decisionModel.modelId] ?? 0 : 0, esc: escalated(k.resolvers, j.decisionModel?.modelId), reused: Object.keys(k.reused ?? {}).length, notes: k.notes }));
}

// mulberry32: a small seeded generator, so a seed replays the arm order exactly.
const seeded = (seed: number) => () => {
  seed = (seed + 0x6d2b79f5) | 0;
  let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
  t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
};

/** Each arm's place (1 first) per repeat and case, shuffled by the seed: over cases and repeats every arm takes every place about equally. */
function placesOf(arms: string[], cases: number, repeat: number, seed: number): (rep: number, c: number, arm: string) => number {
  const random = seeded(seed);
  const places = new Map<string, number>();
  for (let rep = 1; rep <= repeat; rep++) {
    for (let c = 0; c < cases; c++) {
      const order = [...arms];
      for (let i = order.length - 1; i > 0; i--) {
        const k = Math.floor(random() * (i + 1));
        [order[i], order[k]] = [order[k], order[i]];
      }
      order.forEach((a, i) => places.set(`${rep}\t${c}\t${a}`, i + 1));
    }
  }
  return (rep, c, arm) => places.get(`${rep}\t${c}\t${arm}`)!;
}

// A reuse arm's cases of one recorded session, per repeat, are one batch in version order; any other job is a batch of its own.
// Batches run repeat by repeat and case by case, the arms of a case in their seeded places; a reuse batch takes its first case's.
function batches(prepared: Prepared[], o: Options, seed: number): Job[][] {
  const place = placesOf(o.arms.map((a) => a.label), o.cases.length, o.repeat, seed);
  const out = new Map<string, Job[]>();
  for (let rep = 1; rep <= o.repeat; rep++) {
    for (const p of prepared) {
      const key = p.reuse ? `${rep}\t${p.arm}\t${p.c.session ?? p.c.case}` : String(out.size);
      out.set(key, [...(out.get(key) ?? []), { ...p, rep, pos: 0 }]);
    }
  }
  const at = (b: Job[]) => o.cases.indexOf(b[0].c);
  const sorted = [...out.values()].map((b) => b.sort((x, y) => (x.c.version ?? 0) - (y.c.version ?? 0)));
  for (const b of sorted) for (const j of b) j.pos = place(j.rep, at(b), j.arm);
  return sorted.sort((x, y) => x[0].rep - y[0].rep || at(x) - at(y) || x[0].pos - y[0].pos);
}

// A child conversation of the replay's task, so a batch's reuse store holds that batch's answers and no other's.
async function storeOf(api: ToolExecutionApi, ctx: Context): Promise<ToolExecutionApi> {
  const id = await api.commit(async (tx) => (await tx.createConversation({ ownership: { kind: 'task', taskId: api.taskId } })).id, ctx);
  return withKey(api, 'conversationId', id);
}

/** Judges every case under every arm `repeat` times and returns one row per check, and the cases an arm declined. */
export async function replayJudge(o: Options): Promise<Replayed> {
  if (!o.models.getAllModels(o.judgeModel.provider).some((m) => m.id === o.judgeModel.modelId)) throw new Error(`judge ${o.judgeModel.provider}/${o.judgeModel.modelId} is not configured in this deployment`);
  const all = await Promise.all(o.arms.flatMap((arm) => o.cases.map((c) => prepare(arm, c))));
  const declined = all.filter((p): p is Declined => 'reason' in p);
  const prepared = all.filter((p): p is Prepared => !('reason' in p));
  if (!prepared.length) throw new Error(`every case was declined: ${declined.map((d) => `${d.arm} ${d.case}: ${d.reason}`).join('; ')}`);
  // A watched check is valid if at least one arm's rubric has it, so a new check can run alongside main.
  for (const c of o.cases) {
    const forCase = c.checks?.length ? prepared.filter((p) => p.c === c) : [];
    const missing = forCase.length ? c.checks!.filter((n) => !forCase.some((p) => p.found.has(n))) : [];
    if (missing.length) throw new Error(`${c.case}: no check ${missing.join(', ')} in any arm`);
  }
  const seed = o.seed ?? randomInt(2 ** 31);
  const queue = batches(prepared, o, seed);
  const rows: Row[] = [];
  // A tool error only reaches the scripted driver as text, so keep the first one and rethrow it after the run.
  let failure: unknown;
  const replay = defineTool({
    name: 'replay_judge',
    description: 'Judge every recorded case again.',
    parameters: obj({}, { open: true }),
    async execute(_args, api, ctx) {
      await Promise.all(Array.from({ length: o.concurrency }, async () => {
        for (let b = queue.shift(); b && !failure; b = queue.shift()) {
          try {
            const on = b[0].reuse ? await storeOf(api, ctx) : api;
            for (const j of b) {
              const done = await judgeJob(on, ctx, o, j);
              rows.push(...done);
              o.onRows?.(done);
            }
          } catch (err) {
            failure ??= err;
          }
        }
      }));
      return { output: [{ type: 'text', text: 'done' }] };
    },
  });
  const dir = mkdtempSync(join(tmpdir(), 'quack-judge-replay-'));
  try {
    await driveTool(o.models, dir, replay, 'judge-replay');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
  if (failure) throw failure;
  return { rows, declined, seed };
}

const sorted = (xs: number[]) => [...xs].sort((a, b) => a - b);
const median = (xs: number[]) => (xs.length ? sorted(xs)[Math.floor((xs.length - 1) / 2)] : 0);
const p90 = (xs: number[]) => (xs.length ? sorted(xs)[Math.ceil(xs.length * 0.9) - 1] : 0);
const fmt = (m: number | null) => (m === null ? 'null' : String(Math.round(m * 100) / 100));
const keyOf = (r: Row) => `${r.case}\t${r.check}`;
const of = (rows: Row[], arm: string, key: string) => rows.filter((r) => r.arm === arm && keyOf(r) === key).sort((a, b) => a.rep - b.rep);

const secs = (ms: number[]) => `median ${(median(ms) / 1000).toFixed(1)} p90 ${(p90(ms) / 1000).toFixed(1)}`;
const sum = (xs: number[]) => xs.reduce((n, x) => n + x, 0);

/** One arm's counts, then its judge seconds per case overall and by place; a job's checks share its time and requests, so a job counts once. */
function armLine(rows: Row[], arm: string): string {
  const mine = rows.filter((r) => r.arm === arm);
  const jobs = [...new Map(mine.map((r) => [`${r.rep}:${r.case}`, r])).values()];
  const requests: Record<string, number> = {};
  for (const j of jobs) for (const [m, n] of Object.entries(j.requests)) requests[m] = (requests[m] ?? 0) + n;
  const byModel = Object.entries(requests).map(([m, n]) => `${m} ${n}`).join(', ');
  const places = [...new Set(jobs.map((j) => j.pos))].sort((a, b) => a - b);
  const placed = places.map((p) => `${p}: ${secs(jobs.filter((j) => j.pos === p).map((j) => j.ms))}`).join('; ');
  const items = `items decision model ${sum(mine.map((r) => r.clef))}, escalated ${sum(mine.map((r) => r.esc))}, reused ${sum(mine.map((r) => r.reused))}`;
  return `- ${arm}: requests ${sum(Object.values(requests))}${byModel ? ` (${byModel})` : ''}; ${items}; judge seconds per case ${secs(jobs.map((j) => j.ms))}, by place ${placed}`;
}

/** Per arm, the counts load and prefix caching cannot bias come before the seconds they can. */
const perArm = (rows: Row[], arms: string[]) => ['per arm (requests and settled items, then judge seconds per case):', ...arms.map((a) => armLine(rows, a))];

/** Every (case, check) whose met values differ between arms (repeat order aside), with each arm's values and the judge's notes, for a person to adjudicate. */
function disagreements(rows: Row[], arms: string[], keys: string[]): string[] {
  const out: string[] = [];
  for (const key of keys) {
    const per = arms.map((a) => ({ a, rs: of(rows, a, key) })).filter((x) => x.rs.length);
    const met = per.map((x) => sorted(x.rs.map((r) => r.met ?? -1)).join());
    if (new Set(met).size < 2) continue;
    out.push(`- ${key.replace('\t', ' ')}: ${per.map((x) => `${x.a} ${x.rs.map((r) => fmt(r.met)).join(' ')}`).join('; ')}`);
    for (const x of per) for (const n of new Set(x.rs.flatMap((r) => r.notes))) out.push(`  - ${x.a}: ${n}`);
  }
  return out.length ? ['', 'disagreements (case check: met per repeat by arm, then the notes each arm gave):', ...out] : [];
}

/** The share of a check's asked items it may escalate before it reads as unhealthy. */
const ESCALATION_BUDGET = 0.1;

type Count = { key: string; clef: number; esc: number; route?: Route };

// A key never asked of the decision model (declared off it, or an arm without one) cannot move by fixing the check, so it is listed apart and never fails.
// Without a route (an export, an old rows file) a key the decision model settled nothing for may be either, so it is listed apart as no-clef.
function markOf(r: Count & { rate: number }) {
  if (r.route === 'declared' || r.route === 'not-asked') return r.route.padEnd(9);
  if (!r.route && !r.clef) return 'no-clef  ';
  return r.rate > ESCALATION_BUDGET ? 'UNHEALTHY' : 'ok       ';
}

/** Per key, escalated / (decision model + escalated), highest first; asked keys over the budget are UNHEALTHY, and keys nothing was asked of are left out. */
export function budget(counts: Count[]): { lines: string[]; unhealthy: number } {
  const by = new Map<string, Count>();
  for (const c of counts) {
    const t = by.get(c.key) ?? { key: c.key, clef: 0, esc: 0 };
    by.set(c.key, { key: c.key, clef: t.clef + c.clef, esc: t.esc + c.esc, route: t.route ?? c.route });
  }
  const rated = [...by.values()].filter((t) => t.clef + t.esc).map((t) => ({ ...t, rate: t.esc / (t.clef + t.esc) })).sort((a, b) => b.rate - a.rate || a.key.localeCompare(b.key));
  const lines = rated.map((r) => `${markOf(r)} ${`${Math.round(r.rate * 100)}%`.padStart(4)} ${r.esc}/${r.clef + r.esc} ${r.key}`);
  return { lines, unhealthy: lines.filter((l) => l.startsWith('UNHEALTHY')).length };
}

/** The replay table: per case and check, met per repeat and counts per arm; `reportChecks` filters displayed rows while keeping full-round per-arm timing and counts. */
export function table({ rows, declined, seed }: Replayed, arms: string[], reportChecks?: string[]): string {
  const shown = reportChecks?.length ? rows.filter((r) => reportChecks.includes(r.check)) : rows;
  const keys = [...new Set(shown.map(keyOf))].sort();
  const cell = (arm: string, key: string) => {
    const rs = of(shown, arm, key);
    return [rs.map((r) => fmt(r.met)).join(' '), `${rs.reduce((n, r) => n + r.clef, 0)}/${rs.reduce((n, r) => n + r.esc, 0)}`];
  };
  const head = ['case', 'check', ...arms.map((a) => `met ${a}`), ...arms.map((a) => `clef/esc ${a}`)];
  const lines = keys.map((key) => {
    const cells = arms.map((a) => cell(a, key));
    return [...key.split('\t'), ...cells.map((c) => c[0]), ...cells.map((c) => c[1])];
  });
  const md = [head, head.map(() => '---'), ...lines].map((l) => `| ${l.join(' | ')} |`);
  const skipped = declined.length ? ['', 'declined:', ...declined.map((d) => `- ${d.arm} ${d.case}: ${d.reason}`)] : [];
  const seedLine = seed === undefined ? [] : [`arm order seed ${seed}`];
  return [...md, '', ...perArm(rows, arms), ...seedLine, ...disagreements(shown, arms, keys), ...skipped].join('\n');
}
