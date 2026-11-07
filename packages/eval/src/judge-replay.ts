import { execFileSync, spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { MutableModels } from '@earendil-works/pi-ai';
import { obj } from '@earendil-works/pi-schema';
import type { Context } from '@earendil-works/chord/context';
import { defineTool, type ToolExecutionApi } from '@earendil-works/pi-durable';
import { rejudge, type ClassifierRef, type Evidence, type ModelRef, type Rubric } from '@quack/judge';
import type { EvalAdapter, RecordedSession } from '@quack/sdk';
import { driveTool } from './driver.ts';
import { readSession } from './record.ts';

/** A recorded artifact to judge: from a session directory, or with its content and evidence given inline (for a kind whose adapter cannot rebuild evidence). */
export type Case = { case: string; kind: string; checks?: string[]; session?: string; version?: number; content?: string; evidence?: Evidence };
/** An arm judges with its adapters' rubrics and evidence and, when it has one, a per-unit classifier before the judge model. */
export type Arm = { label: string; adapters: EvalAdapter[]; classifier?: ClassifierRef };

/** A tar of `packages/` as it stood at ref in the repo at `top`; a ref from before the repo-root layout keeps it under v1/. */
export function packagesTar(top: string, ref: string): Buffer {
  const base = spawnSync('git', ['cat-file', '-e', `${ref}:packages`], { cwd: top }).status === 0 ? '' : 'v1';
  return execFileSync('git', ['archive', '--format=tar', `${ref}:${base}`, 'packages'], { cwd: top, maxBuffer: 1 << 30 });
}
export type Row = { arm: string; rep: number; case: string; check: string; met: number | null; clef: number; esc: number; ms: number; notes: string[] };
/** A case an arm could not judge, with what its recording lacks. */
export type Declined = { arm: string; case: string; reason: string };
export type Replayed = { rows: Row[]; declined: Declined[] };
export type Options = { cases: Case[]; arms: Arm[]; repeat: number; concurrency: number; models: MutableModels; judgeModel: ModelRef };

const ESCALATED = ['judge-model', 'agent-model'];
type Job = { arm: string; classifier?: ClassifierRef; rep: number; c: Case; rubric: Rubric; content: string; evidence: Evidence };

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
  const record: RecordedSession = c.session ? await readSession(c.session) : { docs: {}, artifacts: {} };
  const versions = record.artifacts[c.kind] ?? [];
  const content = c.content ?? (c.version ? versions.find((v) => v.version === c.version) : versions.at(-1))?.content;
  if (content === undefined) throw new Error(`${c.case}: no ${c.kind} content in the case or its session`);
  // Adapters find the judged version by content, so they see the kind only up to it: a later version with the same content is not it.
  const upTo = c.version ? { ...record, artifacts: { ...record.artifacts, [c.kind]: versions.filter((v) => v.version <= c.version!) } } : record;
  const reasons: string[] = [];
  for (const a of arm.adapters.filter((x) => x.kind === c.kind && x.judge)) {
    const evidence = c.evidence ?? (await a.judge!.evidence(upTo, content).catch((err: Error) => void reasons.push(err.message)));
    if (evidence) return { arm: arm.label, classifier: arm.classifier, c, content, evidence: evidence as Evidence, rubric: watch(a.judge!.rubric as Rubric, c.checks) };
  }
  return { arm: arm.label, case: c.case, reason: reasons.join('; ') || `no adapter judges ${c.kind} with this evidence` };
}

async function judgeJob(api: ToolExecutionApi, ctx: Context, o: Options, j: Job): Promise<Row[]> {
  const sealed = { kind: j.c.kind, version: 1, hash: '', content: j.content, sealedAt: 0, verdict: null };
  const opts = { kind: { kind: j.c.kind, path: 'x' }, rubric: j.rubric, evidence: async () => j.evidence, maxRevisions: 0, classifier: j.classifier, judgeModel: o.judgeModel };
  const t = Date.now();
  const v = await rejudge(api, opts, sealed, `${j.arm}:${j.rep}:${j.c.case}`, ctx);
  const ms = Date.now() - t;
  // A check no resolver could decide is scored unmet by the judge; the table shows it as null so a model outage is not read as a failed check.
  return v.checks.map((k) => ({ arm: j.arm, rep: j.rep, case: j.c.case, check: k.id, met: k.resolvers.none ? null : k.met, clef: j.classifier ? k.resolvers[j.classifier.modelId] ?? 0 : 0, esc: ESCALATED.reduce((n, r) => n + (k.resolvers[r] ?? 0), 0), ms, notes: k.notes }));
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

/** The #1956 table: per case and check, met per repeat and classifier/escalation counts for each arm; then judge seconds per arm, the disagreements and the declined cases. */
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
