// Round 2 tables: agreement with recorded verdicts on rejected and passed versions, and fan-out against today's judge-pass timing.
// Usage: node tools/spike-fanout/tables2.ts
import { readFileSync } from 'node:fs';

const KINDS = ['contracts', 'sleeper', 'research', 'reviews', 'sibling'];
// A recorded verdict is comparable only when its rubric is the one the declarations were written from.
const LATEST: Record<string, number> = { 'sleeper.lineup': 4, 'sleeper.waivers': 4, 'sleeper.history': 1, 'sleeper.trends': 1, 'sleeper.digest': 2, 'sleeper.retro': 2, 'github.reviewer': 4, 'research.report': 3, 'contract.spec': 1 };
type Check = { check: string; chunks: number; decided: boolean; fanFlagged: boolean; fanFlaggedDecided?: boolean; recordedFlagged?: boolean; recordedMet?: number | null; resolvers?: Record<string, number>; unknown: number; oversize: number; tokens: number[]; items: Array<{ text: string; a: any }> };
type Run = { rubric: string; item: string; kind: string; key: string; version: number; passed: boolean; score: number; pass: { requests: number; clefMs: number; localMs: number; wallMs: number }; checks: Check[]; skipped?: string };
const runs: Run[] = KINDS.flatMap((k) => { try { return JSON.parse(readFileSync(`tools/spike-fanout/out/round2-${k}.json`, 'utf8')) as Run[]; } catch { return []; } }).filter((r) => !r.skipped);
const current = (r: Run) => LATEST[r.rubric.split('@')[0]] === Number(r.rubric.split('@')[1]);
const stale = runs.filter((r) => !current(r));

const pct = (n: number, d: number) => (d ? `${Math.round((100 * n) / d)}% (${n}/${d})` : 'n/a');
const med = (a: number[]) => (a.length ? [...a].sort((x, y) => x - y)[Math.floor((a.length - 1) / 2)] : 0);
const mm = (a: number[], f = (x: number) => x.toFixed(1)) => (a.length ? `${f(med(a))} / ${f(Math.max(...a))}` : 'n/a');

const out: string[] = [];
out.push(`Versions run: ${runs.length} (${stale.length} recorded under an older rubric are left out of the agreement table) ; (${runs.filter((r) => !r.passed).length} rejected, ${runs.filter((r) => r.passed).length} passed).`);

// Agreement: a check is comparable when the fan-out had at least one unit and the recorded verdict scored it.
const rows = ['| kind | check | recorded verdicts | n | both flag | only recorded | only fan-out | neither | fan-out undecided |', '| --- | --- | --- | --- | --- | --- | --- | --- | --- |'];
const keyOf = (r: Run) => (r.kind === 'sleeper.cards' ? `sleeper.cards ${r.key.split(':').pop()}` : r.kind);
const groups = new Map<string, Array<{ r: Run; c: Check }>>();
for (const r of runs.filter(current)) for (const c of r.checks) if (c.recordedFlagged !== undefined && c.chunks > 0) {
  const k = `${keyOf(r)}|${c.check}|${r.passed ? 'passed' : 'rejected'}`;
  groups.set(k, [...(groups.get(k) ?? []), { r, c }]);
}
for (const [k, xs] of [...groups].sort()) {
  const [kind, check, which] = k.split('|');
  const both = xs.filter((x) => x.c.recordedFlagged && x.c.fanFlagged).length;
  const rec = xs.filter((x) => x.c.recordedFlagged && !x.c.fanFlagged).length;
  const fan = xs.filter((x) => !x.c.recordedFlagged && x.c.fanFlagged).length;
  rows.push(`| ${kind} | ${check} | ${which} | ${xs.length} | ${both} | ${rec} | ${fan} | ${xs.length - both - rec - fan} | ${xs.filter((x) => !x.c.decided).length} |`);
}
out.push(rows.join('\n'));

// Recorded flags the fan-out could not even raise because no unit exists (sibling_parts without a new same-suffix file).
const noUnit = runs.flatMap((r) => r.checks.filter((c) => c.recordedFlagged && c.chunks === 0).map((c) => `${r.item.split('|')[1].replace('github:fagerbergj/quack', '')} v${r.version} ${c.check}`));
out.push(`Recorded flags with no fan-out unit: ${noUnit.length}\n${noUnit.join(', ')}`);

// Timing.
type Task = { key: string; name: string; startedAt: number; endedAt: number; missing?: boolean };
const tasks: Task[] = [];
for (const f of ['times.jsonl', 'times-sleeper_.jsonl']) { try { tasks.push(...readFileSync(`tools/spike-fanout/out/${f}`, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l))); } catch { /* optional */ } }
const dur = (names: string[], min = 0) => [...new Map(tasks.filter((t) => names.includes(t.name) && t.endedAt && t.endedAt - t.startedAt >= min).map((t) => [`${t.key}${t.startedAt}`, (t.endedAt - t.startedAt) / 1000])).values()];
const TODAY: Record<string, string[]> = { 'github.review': ['submit_review'], 'research.report': ['submit_report'], 'contract.spec': ['submit_contract'], 'sleeper.cards': ['workflow:quack.workflow.sleeper.lineup', 'workflow:quack.workflow.sleeper.waivers', 'workflow:quack.workflow.sleeper.history', 'workflow:quack.workflow.sleeper.trends', 'workflow:quack.workflow.sleeper.digest', 'workflow:quack.workflow.sleeper.retro'] };
const trows = ['| kind | versions run | Clef calls per artifact (median / max) | fan-out pass, concurrency 1 (median / max, s) | of which code (splitter + fold, ms) | today: judge-pass tasks (n) | today: all passes (median / max, s) | today: passes over 3 s (median / max, s) | ratio of medians (today over 3 s / fan-out) |', '| --- | --- | --- | --- | --- | --- | --- | --- | --- |'];
for (const kind of Object.keys(TODAY)) {
  const rs = runs.filter((r) => r.kind === kind);
  if (!rs.length) continue;
  const fan = rs.map((r) => (r.pass.clefMs + r.pass.localMs) / 1000);
  const all = dur(TODAY[kind]);
  const real = dur(TODAY[kind], 3000);
  trows.push(`| ${kind} | ${rs.length} | ${mm(rs.map((r) => r.pass.requests), (x) => String(Math.round(x)))} | ${mm(fan)} | ${mm(rs.map((r) => r.pass.localMs), (x) => x.toFixed(0))} | ${all.length} | ${mm(all)} | ${mm(real)} | ${real.length && fan.length ? (med(real) / med(fan)).toFixed(0) + 'x' : 'n/a'} |`);
}
out.push(trows.join('\n'));
console.log(out.join('\n\n'));
