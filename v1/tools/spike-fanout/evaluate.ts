// Runs a declaration over one artifact's inputs, asks Clef one request per chunk and scope, and folds the answers.
import { ask, BYTES_PER_TOKEN, MAX_INPUT_TOKENS, type Reply } from './clef.ts';
import { fold, tri, unitsOf, type Answer, type Declaration, type Folded, type Inputs, type Unit } from './strategies.ts';

export type CheckRun = { artifact: string; check: string; splitter: string; reason?: string; rejected: number; swept: number; texts: string[]; chunks: number; tokens: number[]; ms: number[]; oversize: number; decided: boolean[]; folded: Folded; related: number[]; answers: Array<Answer | undefined> };

/** What the last evaluate call cost: Clef requests, their summed server-side time, and the splitter and fold time spent in code. */
export const pass = { requests: 0, clefMs: 0, localMs: 0 };

export async function evaluate(decl: Declaration, artifact: string, inputs: Inputs, live = true): Promise<CheckRun[]> {
  const out: CheckRun[] = [];
  pass.requests = pass.clefMs = pass.localMs = 0;
  for (const [sid, scope] of Object.entries(decl.scopes)) {
    const checks = Object.entries(decl.checks).filter(([, c]) => c.scope === sid);
    if (!checks.length || (process.env.SKIP && new RegExp(process.env.SKIP).test(sid))) continue;
    let t0 = performance.now();
    const { units, splitter, reason, rejected, swept } = unitsOf(scope, inputs);
    pass.localMs += performance.now() - t0;
    const questions = Object.fromEntries(checks.map(([id, c]) => [c.wire ?? id, c.question]));
    const replies: Reply[] = [];
    for (const u of units) replies.push(live ? await ask(u.state, questions) : estimate(u, questions));
    pass.requests += replies.filter((r) => !r.oversize).length;
    pass.clefMs += replies.reduce((a, r) => a + r.ms, 0);
    t0 = performance.now();
    for (const [id, c] of checks) {
      const answers = replies.map((r) => r.answers[c.wire ?? id]);
      const related = units.map((u) => Object.values(u.related).reduce((a, b) => a + b, 0));
      out.push({ artifact, check: id, splitter, reason, rejected, swept, texts: units.map((u) => Object.values(u.state)[0]), chunks: units.length, tokens: replies.map((r) => r.tokens), ms: replies.map((r) => r.ms), oversize: replies.filter((r) => r.oversize).length, decided: answers.map((a) => tri(a, c.aggregate) !== undefined), folded: fold({ ...c.aggregate }, units.map((_, i) => ({ answer: answers[i], related: related[i] }))), related, answers });
    }
    pass.localMs += performance.now() - t0;
  }
  return out;
}

const estimate = (u: Unit, questions: object): Reply => {
  const tokens = Math.ceil(JSON.stringify({ state: u.state, questions }).length / BYTES_PER_TOKEN);
  return { answers: {}, ms: 0, tokens, oversize: tokens > MAX_INPUT_TOKENS, cached: false };
};

