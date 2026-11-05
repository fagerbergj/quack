// One-at-a-time Clef client over an ssh pipe into the quack-v1 container (bridge.js), with an on-disk cache and a hard request budget.
import { spawn, type ChildProcessWithoutNullStreams } from 'node:child_process';
import { appendFileSync, existsSync, readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import type { Answer, Question } from './strategies.ts';

export const BYTES_PER_TOKEN = 4;
export const MAX_INPUT_TOKENS = 8192;
const CACHE = 'tools/spike-fanout/out/clef-cache.jsonl';
const BUDGET = Number(process.env.CLEF_BUDGET ?? 380);

export type Reply = { answers: Record<string, Answer>; ms: number; tokens: number; oversize: boolean; cached: boolean };
let child: ChildProcessWithoutNullStreams | undefined;
let sent = 0;
const waiting = new Map<string, (r: any) => void>();
const cache = new Map<string, Reply>();
if (existsSync(CACHE)) for (const l of readFileSync(CACHE, 'utf8').split('\n').filter(Boolean)) { const r = JSON.parse(l); cache.set(r.key, r.reply); }

export const requestsSent = () => sent;

function start() {
  const code = readFileSync('tools/spike-fanout/bridge.js', 'utf8');
  const b64 = Buffer.from(code).toString('base64');
  child = spawn('ssh', ['jason-server@jason-server.hs.internal', `docker exec -i quack-v1 node -e "eval(Buffer.from('${b64}','base64').toString())"`]);
  let buf = '';
  child.stdout.on('data', (d) => {
    buf += d;
    for (let i = buf.indexOf('\n'); i >= 0; i = buf.indexOf('\n')) {
      const r = JSON.parse(buf.slice(0, i));
      buf = buf.slice(i + 1);
      waiting.get(r.id)?.(r);
    }
  });
  child.stderr.on('data', (d) => process.stderr.write(`bridge: ${d}`));
}

export function stop() { child?.stdin.end(); child = undefined; }

const wire = (q: Question) => (q.type === 'bool' ? { type: 'noul', instructions: q.instructions, criteria: { true: 'Yes, this holds.', false: 'No, it does not hold.' } } : { type: 'choice', instructions: q.instructions, criteria: q.options });

/** A request Clef's own tokenizer rejects as too large (it is denser than the judge's 4-bytes-per-token rule on code) escalates, like an oversize one. */
function tooLarge(r: any, key: string, tokens: number): Reply | undefined {
  const text = JSON.stringify(r.json);
  if (r.status !== 500 || !text.includes('too large')) return undefined;
  const big: Reply = { answers: {}, ms: r.ms, tokens: Number(/\((\d+) tokens\)/.exec(text)?.[1] ?? tokens), oversize: true, cached: false };
  cache.set(key, big);
  appendFileSync(CACHE, `${JSON.stringify({ key, reply: big, raw: r.json })}\n`);
  return big;
}

/** Asks the questions about one state in a single request, as the judge does per unit. Oversized states are not sent (the judge escalates them). */
export async function ask(state: Record<string, string>, questions: Record<string, Question>, opts: { par?: boolean; fresh?: boolean } = {}): Promise<Reply> {
  const body = { model: 'clef', state, questions: Object.fromEntries(Object.entries(questions).map(([k, q]) => [k, wire(q)])) };
  const tokens = Math.ceil(JSON.stringify({ state, questions: body.questions }).length / BYTES_PER_TOKEN);
  if (tokens > MAX_INPUT_TOKENS) return { answers: {}, ms: 0, tokens, oversize: true, cached: false };
  const key = createHash('sha256').update(JSON.stringify(body)).digest('hex');
  const hit = opts.fresh ? undefined : cache.get(key);
  if (hit) return { ...hit, cached: true };
  if (sent >= BUDGET) throw new Error(`Clef request budget ${BUDGET} reached`);
  if (!child) start();
  sent++;
  const id = `r${sent}`;
  const r: any = await new Promise((res) => { waiting.set(id, res); child!.stdin.write(`${JSON.stringify({ id, body, par: opts.par })}\n`); });
  const big = tooLarge(r, key, tokens);
  if (big) return big;
  if (r.status !== 200 || !r.json) throw new Error(`Clef ${r.status} ${r.error ?? JSON.stringify(r.json).slice(0, 300)}`);
  const answers: Record<string, Answer> = {};
  for (const [k, q] of Object.entries(questions)) {
    const a = r.json.answers?.[k] ?? r.json[k];
    answers[k] = q.type === 'bool' ? { p: a?.noul } : { probs: a?.probabilities };
  }
  const reply: Reply = { answers, ms: r.ms, tokens, oversize: false, cached: false };
  if (opts.fresh) return reply;
  cache.set(key, reply);
  appendFileSync(CACHE, `${JSON.stringify({ key, reply, raw: r.json })}\n`);
  return reply;
}
