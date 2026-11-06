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
export type Arm = { label: string; adapters: EvalAdapter[] };

/** A tar of `packages/` as it stood at ref in the repo at `top`; a ref from before the repo-root layout keeps it under v1/. */
export function packagesTar(top: string, ref: string): Buffer {
  const base = spawnSync('git', ['cat-file', '-e', `${ref}:packages`], { cwd: top }).status === 0 ? '' : 'v1';
  return execFileSync('git', ['archive', '--format=tar', `${ref}:${base}`, 'packages'], { cwd: top, maxBuffer: 1 << 30 });
}
export type Row = { arm: string; rep: number; case: string; check: string; met: number | null; clef: number; esc: number; ms: number };
export type Options = { cases: Case[]; arms: Arm[]; repeat: number; concurrency: number; models: MutableModels; judgeModel: ModelRef; classifier?: ClassifierRef };

const ESCALATED = ['judge-model', 'agent-model'];
type Job = { arm: string; rep: number; c: Case; rubric: Rubric; content: string; evidence: Evidence };

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

async function prepare(arm: Arm, c: Case): Promise<Omit<Job, 'rep'>> {
  const record: RecordedSession = c.session ? await readSession(c.session) : { docs: {}, artifacts: {} };
  const versions = record.artifacts[c.kind] ?? [];
  const content = c.content ?? (c.version ? versions.find((v) => v.version === c.version) : versions.at(-1))?.content;
  if (content === undefined) throw new Error(`${c.case}: no ${c.kind} content in the case or its session`);
  for (const a of arm.adapters.filter((x) => x.kind === c.kind && x.judge)) {
    const evidence = (c.evidence ?? (await a.judge!.evidence(record, content))) as Evidence | undefined;
    if (evidence) return { arm: arm.label, c, content, evidence, rubric: watch(a.judge!.rubric as Rubric, c.checks) };
  }
  throw new Error(`${c.case}: arm ${arm.label} has no adapter that judges ${c.kind} with this evidence`);
}

async function judgeJob(api: ToolExecutionApi, ctx: Context, o: Options, j: Job): Promise<Row[]> {
  const sealed = { kind: j.c.kind, version: 1, hash: '', content: j.content, sealedAt: 0, verdict: null };
  const opts = { kind: { kind: j.c.kind, path: 'x' }, rubric: j.rubric, evidence: async () => j.evidence, maxRevisions: 0, classifier: o.classifier, judgeModel: o.judgeModel };
  const t = Date.now();
  const v = await rejudge(api, opts, sealed, `${j.arm}:${j.rep}:${j.c.case}`, ctx);
  const ms = Date.now() - t;
  // A check no resolver could decide is scored unmet by the judge; the table shows it as null so a model outage is not read as a failed check.
  return v.checks.map((k) => ({ arm: j.arm, rep: j.rep, case: j.c.case, check: k.id, met: k.resolvers.none ? null : k.met, clef: o.classifier ? k.resolvers[o.classifier.modelId] ?? 0 : 0, esc: ESCALATED.reduce((n, r) => n + (k.resolvers[r] ?? 0), 0), ms }));
}

/** Judges every case under every arm `repeat` times and returns one row per check. */
export async function replayJudge(o: Options): Promise<Row[]> {
  if (!o.models.getAllModels(o.judgeModel.provider).some((m) => m.id === o.judgeModel.modelId)) throw new Error(`judge ${o.judgeModel.provider}/${o.judgeModel.modelId} is not configured in this deployment`);
  const prepared = await Promise.all(o.arms.flatMap((arm) => o.cases.map((c) => prepare(arm, c))));
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
  return rows;
}

const median = (xs: number[]) => (xs.length ? [...xs].sort((a, b) => a - b)[Math.floor((xs.length - 1) / 2)] : 0);
const fmt = (m: number | null) => (m === null ? 'null' : String(Math.round(m * 100) / 100));

/** The #1956 table: per case and check, met per repeat and classifier/escalation counts for each arm, then median judge seconds per arm. */
export function table(rows: Row[], arms: string[]): string {
  const keys = [...new Set(rows.map((r) => `${r.case}\t${r.check}`))].sort();
  const cell = (arm: string, key: string) => {
    const rs = rows.filter((r) => r.arm === arm && `${r.case}\t${r.check}` === key).sort((a, b) => a.rep - b.rep);
    return [rs.map((r) => fmt(r.met)).join(' '), `${rs.reduce((n, r) => n + r.clef, 0)}/${rs.reduce((n, r) => n + r.esc, 0)}`];
  };
  const head = ['case', 'check', ...arms.map((a) => `met ${a}`), ...arms.map((a) => `clef/esc ${a}`)];
  const lines = keys.map((key) => {
    const cells = arms.map((a) => cell(a, key));
    return [...key.split('\t'), ...cells.map((c) => c[0]), ...cells.map((c) => c[1])];
  });
  const md = [head, head.map(() => '---'), ...lines].map((l) => `| ${l.join(' | ')} |`);
  // A job's checks share one wall time, so count it once per (rep, case).
  const secs = arms.map((a) => {
    const jobs = new Map(rows.filter((r) => r.arm === a).map((r) => [`${r.rep}:${r.case}`, r.ms]));
    return `${a} ${(median([...jobs.values()]) / 1000).toFixed(1)}`;
  });
  return [...md, '', `median judge seconds per case: ${secs.join(', ')}`].join('\n');
}
