import { execFileSync, spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readdirSync, rmSync, symlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import type { MutableModels } from '@earendil-works/pi-ai';
import { obj } from '@earendil-works/pi-schema';
import type { Context } from '@earendil-works/chord/context';
import { defineTool, type ToolExecutionApi } from '@earendil-works/pi-durable';
import { rejudge, type DecisionModelRef, type Declaration, type Evidence, type Inputs, type JudgeOptions, type ModelRef, type Rubric } from '@quack/judge';
import { extensionList, loadManifests } from '@quack/runtime/catalog';
import type { EvalAdapter, RecordedSession } from '@quack/sdk';
import { adaptersOf } from './adapters.ts';
import { driveTool } from './driver.ts';
import { asJudged, readSession } from './record.ts';

/** A recorded artifact to judge: from a session directory, or with its content and evidence (or a declared rubric's inputs) given inline, for a kind whose adapter cannot rebuild them; `units` keeps only those evidence units. */
export type Case = { case: string; kind: string; checks?: string[]; units?: string[]; session?: string; version?: number; content?: string; evidence?: Evidence; inputs?: Inputs };
/** An arm judges with its adapters' rubrics and evidence, its judge engine (this tree's when unset) and, when it has one, a per-unit decision model before the judge model. */
export type Arm = { label: string; adapters: EvalAdapter[]; rejudge?: typeof rejudge; decisionModel?: DecisionModelRef };

/** A tar of `packages/` as it stood at ref in the repo at `top`; a ref from before the repo-root layout keeps it under v1/. */
export function packagesTar(top: string, ref: string): Buffer {
  const base = spawnSync('git', ['cat-file', '-e', `${ref}:packages`], { cwd: top }).status === 0 ? '' : 'v1';
  return execFileSync('git', ['archive', '--format=tar', `${ref}:${base}`, 'packages'], { cwd: top, maxBuffer: 1 << 30 });
}

/** An arm's adapters and judge engine as they stood at ref (`.` is this tree). */
export async function armAt(ref: string): Promise<Pick<Arm, 'adapters' | 'rejudge'>> {
  if (ref === '.') return { adapters: adaptersOf(await loadManifests()) };
  const dir = mkdtempSync(join(tmpdir(), 'quack-arm-'));
  try {
    const top = execFileSync('git', ['rev-parse', '--show-toplevel'], { encoding: 'utf8' }).trim();
    execFileSync('tar', ['-x', '-C', dir], { input: packagesTar(top, ref) });
    return await loadArm(dir);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

/** The adapters and engine of the `packages/` tree in dir, whose @quack/* imports resolve inside dir; third-party modules stay this tree's, shared with the harness. */
export async function loadArm(dir: string): Promise<Pick<Arm, 'adapters' | 'rejudge'>> {
  const modules = fileURLToPath(new URL('../../../node_modules', import.meta.url));
  mkdirSync(join(dir, 'node_modules/@quack'), { recursive: true });
  for (const m of readdirSync(modules).filter((m) => m !== '@quack')) symlinkSync(join(modules, m), join(dir, 'node_modules', m));
  for (const p of readdirSync(join(dir, 'packages'))) {
    symlinkSync(join(dir, 'packages', p), join(dir, 'node_modules/@quack', p));
    // npm nests a dependency under its workspace when the root holds a conflicting version.
    const nested = join(modules, '../packages', p, 'node_modules');
    if (existsSync(nested)) symlinkSync(nested, join(dir, 'packages', p, 'node_modules'));
  }
  const at = (pkg: string) => pathToFileURL(join(dir, 'packages', pkg, 'src/index.ts')).href;
  const specs = extensionList().filter((s) => s.startsWith('@quack/')).map((s) => at(s.slice(7)));
  const engine = (await import(at('judge'))) as typeof import('@quack/judge');
  return { adapters: adaptersOf(await loadManifests(specs)), rejudge: engine.rejudge };
}
export type Row = { arm: string; rep: number; case: string; check: string; met: number | null; clef: number; esc: number; ms: number; notes: string[] };
/** A case an arm could not judge, with what its recording lacks. */
export type Declined = { arm: string; case: string; reason: string };
export type Replayed = { rows: Row[]; declined: Declined[] };
/** `escalations` bounds each job's concurrent judge-model calls (the judge's default when unset), so the judge model sees at most concurrency × escalations. */
export type Options = { cases: Case[]; arms: Arm[]; repeat: number; concurrency: number; models: MutableModels; judgeModel: ModelRef; escalations?: number };

const ESCALATED = ['judge-model', 'agent-model'];
/** The options that route a job's questions; every arm's engine must read them under these names. */
const ROUTING = ['decisionModel', 'judgeModel', 'escalations'] as const;
/** What a job is judged on: a declared rubric's declaration and inputs, or an undeclared one's evidence. */
type Judged = { evidence: Evidence } | { declaration: Declaration; inputs: Inputs };
type Job = { arm: string; rejudge: typeof rejudge; decisionModel?: DecisionModelRef; rep: number; c: Case; rubric: Rubric; content: string; judged: Judged };

/** The rubric cut to the watched checks (by id or criterion.id), so only those are judged; a name that matches nothing is an error. */
function watch(rubric: Rubric, names: string[] | undefined): Rubric {
  if (!names?.length) return rubric;
  const hit = (cid: string, id: string) => names.includes(id) || names.includes(`${cid}.${id}`);
  const criteria = rubric.criteria.map((c) => ({ ...c, checks: c.checks.filter((k) => hit(c.id, k.id)) })).filter((c) => c.checks.length);
  const found = criteria.flatMap((c) => c.checks.flatMap((k) => [k.id, `${c.id}.${k.id}`]));
  const missing = names.filter((n) => !found.includes(n));
  if (missing.length) throw new Error(`${rubric.name}@${rubric.version} has no check ${missing.join(', ')}`);
  return { ...rubric, criteria };
}

// An adapter that throws is declining the case for a reason (what the recording lacks); a case file without content is an error.
async function prepare(arm: Arm, c: Case): Promise<Omit<Job, 'rep'> | Declined> {
  // Adapters find the judged version by content, so they see the session as it was judged: a later version with the same content is not it.
  const record: RecordedSession = c.session ? asJudged(await readSession(c.session), c.kind, c.version) : { docs: {}, artifacts: {} };
  const versions = record.artifacts[c.kind] ?? [];
  const content = c.content ?? (c.version ? versions.find((v) => v.version === c.version) : versions.at(-1))?.content;
  if (content === undefined) throw new Error(`${c.case}: no ${c.kind} content in the case or its session`);
  const reasons = new Set<string>();
  for (const a of arm.adapters.filter((x) => x.kind === c.kind && x.judge)) {
    const judged = await judgedBy(a, c, record, content).catch((err: Error) => void reasons.add(err.message));
    if (!judged) continue;
    const rubric = watch(a.judge!.rubric as Rubric, c.checks);
    return { arm: arm.label, rejudge: arm.rejudge ?? rejudge, decisionModel: arm.decisionModel, c, content, rubric, judged: 'evidence' in judged ? judged : { ...judged, declaration: declared(judged.declaration, rubric, c.units) } };
  }
  return { arm: arm.label, case: c.case, reason: [...reasons].join('; ') || `no adapter judges ${c.kind} with this evidence` };
}

async function judgedBy(a: EvalAdapter, c: Case, record: RecordedSession, content: string): Promise<Judged | undefined> {
  const j = a.judge!;
  if (j.declaration) return { declaration: j.declaration as Declaration, inputs: c.inputs ?? (await j.inputs!(record, content)) };
  const evidence = (c.evidence ?? (await j.evidence!(record, content))) as Evidence | undefined;
  // One finding of a 50-finding report is a case of its own without judging the other 49 every repeat.
  const units = c.units;
  return evidence ? { evidence: units ? Object.fromEntries(Object.entries(evidence).map(([scope, us]) => [scope, us.filter((u) => units.includes(u.id))])) : evidence } : undefined;
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

async function judgeJob(api: ToolExecutionApi, ctx: Context, o: Options, j: Job): Promise<Row[]> {
  const sealed = { kind: j.c.kind, version: 1, hash: '', content: j.content, sealedAt: 0, verdict: null };
  const base = { kind: { kind: j.c.kind, path: 'x' }, rubric: j.rubric, maxRevisions: 0, decisionModel: j.decisionModel, judgeModel: o.judgeModel, escalations: o.escalations };
  const judged = j.judged;
  const opts: JudgeOptions = 'evidence' in judged ? { ...base, evidence: async () => judged.evidence } : { ...base, declaration: judged.declaration, inputs: async () => judged.inputs };
  const read = new Set<PropertyKey>();
  const watched = new Proxy(opts, { get: (target, key, receiver) => (read.add(key), Reflect.get(target, key, receiver)) });
  const t = Date.now();
  const v = await j.rejudge(api, watched, sealed, `${j.arm}:${j.rep}:${j.c.case}`, ctx);
  const ms = Date.now() - t;
  // An engine that names a routing option otherwise (before #2290, `classifier`) would judge without it and say nothing.
  const ignored = ROUTING.filter((k) => opts[k] !== undefined && !read.has(k));
  if (ignored.length) throw new Error(`arm ${j.arm}: its judge engine never reads ${ignored.join(', ')}, so it would judge without them; run that arm from a ref whose judge takes these options, or from its own worktree`);
  // An undecided check is already met null; a decided fold reads as 1 or 0 beside the shares of undeclared rubrics.
  return v.checks.map((k) => ({ arm: j.arm, rep: j.rep, case: j.c.case, check: k.id, met: typeof k.met === 'boolean' ? Number(k.met) : k.met, clef: j.decisionModel ? k.resolvers[j.decisionModel.modelId] ?? 0 : 0, esc: ESCALATED.reduce((n, r) => n + (k.resolvers[r] ?? 0), 0), ms, notes: k.notes }));
}

/** Judges every case under every arm `repeat` times and returns one row per check, and the cases an arm declined. */
export async function replayJudge(o: Options): Promise<Replayed> {
  if (!o.models.getAllModels(o.judgeModel.provider).some((m) => m.id === o.judgeModel.modelId)) throw new Error(`judge ${o.judgeModel.provider}/${o.judgeModel.modelId} is not configured in this deployment`);
  const all = await Promise.all(o.arms.flatMap((arm) => o.cases.map((c) => prepare(arm, c))));
  const declined = all.filter((p): p is Declined => 'reason' in p);
  const prepared = all.filter((p): p is Omit<Job, 'rep'> => !('reason' in p));
  if (!prepared.length) throw new Error(`every case was declined: ${declined.map((d) => `${d.arm} ${d.case}: ${d.reason}`).join('; ')}`);
  const queue: Job[] = Array.from({ length: o.repeat }, (_, i) => prepared.map((p) => ({ ...p, rep: i + 1 }))).flat();
  const rows: Row[] = [];
  // A tool error only reaches the scripted driver as text, so keep the first one and rethrow it after the run.
  let failure: unknown;
  const replay = defineTool({
    name: 'replay_judge',
    description: 'Judge every recorded case again.',
    parameters: obj({}, { open: true }),
    async execute(_args, api, ctx) {
      await Promise.all(Array.from({ length: o.concurrency }, async () => {
        for (let j = queue.shift(); j && !failure; j = queue.shift()) {
          try {
            rows.push(...(await judgeJob(api, ctx, o, j)));
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
  return { rows, declined };
}

const sorted = (xs: number[]) => [...xs].sort((a, b) => a - b);
const median = (xs: number[]) => (xs.length ? sorted(xs)[Math.floor((xs.length - 1) / 2)] : 0);
const p90 = (xs: number[]) => (xs.length ? sorted(xs)[Math.ceil(xs.length * 0.9) - 1] : 0);
const fmt = (m: number | null) => (m === null ? 'null' : String(Math.round(m * 100) / 100));
const keyOf = (r: Row) => `${r.case}\t${r.check}`;
const of = (rows: Row[], arm: string, key: string) => rows.filter((r) => r.arm === arm && keyOf(r) === key).sort((a, b) => a.rep - b.rep);

/** Each arm's judge seconds per case: a job's checks share one wall time, so it counts once per (rep, case). */
function seconds(rows: Row[], arms: string[]): string {
  const secs = arms.map((a) => {
    const jobs = [...new Map(rows.filter((r) => r.arm === a).map((r) => [`${r.rep}:${r.case}`, r.ms])).values()];
    return `${a} median ${(median(jobs) / 1000).toFixed(1)} p90 ${(p90(jobs) / 1000).toFixed(1)}`;
  });
  return `judge seconds per case: ${secs.join(', ')}`;
}

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

/** The replay table: per case and check, met per repeat and decision-model/escalation counts for each arm; then judge seconds per arm, the disagreements and the declined cases. */
export function table({ rows, declined }: Replayed, arms: string[]): string {
  const keys = [...new Set(rows.map(keyOf))].sort();
  const cell = (arm: string, key: string) => {
    const rs = of(rows, arm, key);
    return [rs.map((r) => fmt(r.met)).join(' '), `${rs.reduce((n, r) => n + r.clef, 0)}/${rs.reduce((n, r) => n + r.esc, 0)}`];
  };
  const head = ['case', 'check', ...arms.map((a) => `met ${a}`), ...arms.map((a) => `clef/esc ${a}`)];
  const lines = keys.map((key) => {
    const cells = arms.map((a) => cell(a, key));
    return [...key.split('\t'), ...cells.map((c) => c[0]), ...cells.map((c) => c[1])];
  });
  const md = [head, head.map(() => '---'), ...lines].map((l) => `| ${l.join(' | ')} |`);
  const skipped = declined.length ? ['', 'declined:', ...declined.map((d) => `- ${d.arm} ${d.case}: ${d.reason}`)] : [];
  return [...md, '', seconds(rows, arms), ...disagreements(rows, arms, keys), ...skipped].join('\n');
}
