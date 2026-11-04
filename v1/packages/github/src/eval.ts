import { randomBytes } from 'node:crypto';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import type { EvalAdapter } from '@quack/sdk';
import type { PullRequestRecord } from './checkout.ts';
import { reviewerRubric } from './rubric.ts';
import { parseReview, reviewKind } from './review.ts';

// A replay's service half: the pinned pull request as recorded, and every review captured, never posted (EVL-2).
// shortcut: no credentials, so replays clone public repositories only; forward read-only tokens when a private repo needs eval.
export async function evalService(pin: { pr: PullRequestRecord }) {
  const token = randomBytes(16).toString('hex');
  const captured: unknown[] = [];
  const server = createServer(async (req, res) => {
    let raw = '';
    for await (const c of req) raw += c;
    const send = (status: number, body: unknown) => res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));
    if (req.headers.authorization !== `Bearer ${token}`) return send(401, { error: 'unauthorized' });
    const { pr } = pin;
    // The session reaches its apps under /apps/<name> and publishes facts to the same base, which the replay discards.
    const path = (req.url ?? '').replace(/^\/apps\/github/, '');
    if (path === '/index/facts') return send(200, {});
    if (req.method === 'GET' && path === `/pulls/${pr.owner}/${pr.repo}/${pr.number}`) return send(200, pr);
    if (req.method === 'POST' && path === '/reviews') {
      const r = JSON.parse(raw);
      // Only a review of the pinned PR at its pinned head counts as captured; anything else would score a delivery that could not happen.
      if (r.owner !== pr.owner || r.repo !== pr.repo || r.number !== pr.number || r.commitId !== pr.headSha) return send(404, { error: 'not the pinned pull request' });
      captured.push(r);
      return send(201, { url: '(eval: not delivered)' });
    }
    send(404, { error: 'not available in eval' });
  }).listen(0, '127.0.0.1');
  await new Promise((r) => server.once('listening', r));
  const url = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  return { env: { QUACK_INDEX_URL: url, QUACK_INDEX_TOKEN: token }, captured, close: () => new Promise<void>((r) => server.close(() => r())) };
}

// shortcut: no judge evidence, since reviewEvidence reads a live checkout a recording does not keep (judge replay takes inline evidence); add one with a pinned diff.
export const evalAdapter: EvalAdapter = {
  kind: reviewKind.kind,
  services: { 'github.pr': (pin) => evalService(pin as never) },
  blocks: (content) => parseReview(content).verdict === 'request_changes',
  judge: { rubric: reviewerRubric, evidence: async () => undefined },
};
