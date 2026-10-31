// The GitHub service half for benchmarks: PR metadata through gh at a pinned head, no credentials, reviews recorded and never posted.
import { execFileSync } from 'node:child_process';
import { appendFileSync } from 'node:fs';
import { createServer } from 'node:http';

const gh = (path: string) => JSON.parse(execFileSync('gh', ['api', path], { encoding: 'utf8', maxBuffer: 64 << 20 }));
const CLOSES = /\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)/gi;

// The description as it stood at `at`: an author who edits it after review would otherwise describe commits the pinned head lacks.
// Edits come newest first, each holding the whole description; an unedited PR has none.
function bodyAt(owner: string, repo: string, n: string, at: string, current: string): string {
  const query = `query{repository(owner:"${owner}",name:"${repo}"){pullRequest(number:${n}){userContentEdits(first:100){nodes{editedAt diff}}}}}`;
  const edits = JSON.parse(execFileSync('gh', ['api', 'graphql', '-f', `query=${query}`], { encoding: 'utf8' })).data.repository.pullRequest.userContentEdits.nodes as Array<{ editedAt: string; diff: string }>;
  const asOf = edits.find((e) => e.editedAt <= at);
  // GitHub lists the original description as the oldest edit, so an edited PR always has one at or before its review.
  if (!asOf && edits.length) throw new Error(`#${n}: no description edit at or before ${at}; refusing to serve the current one`);
  return asOf?.diff ?? current;
}

/** pins maps "owner/repo#n" to the head to review and when it was reviewed; out is where posted reviews are appended. */
export function stubService(pins: Record<string, { head: string; at: string }>, out: string) {
  return createServer(async (req, res) => {
    let raw = '';
    for await (const c of req) raw += c;
    const m = /^\/pulls\/([^/]+)\/([^/]+)\/(\d+)$/.exec(req.url ?? '');
    if (m) {
      const [, owner, repo, n] = m;
      const pr = gh(`/repos/${owner}/${repo}/pulls/${n}`);
      const pin = pins[`${owner}/${repo}#${n}`];
      const headSha = pin?.head ?? pr.head.sha;
      if (pin) pr.body = bodyAt(owner, repo, n, pin.at, pr.body ?? '');
      const checks = gh(`/repos/${owner}/${repo}/commits/${headSha}/check-runs?per_page=100`).check_runs;
      const closes = [...new Set([...String(pr.body ?? '').matchAll(CLOSES)].map((x) => x[1]))];
      const linkedIssues = closes.map((i) => gh(`/repos/${owner}/${repo}/issues/${i}`)).map((i) => ({ number: i.number, title: i.title, body: i.body ?? '' }));
      const record = { owner, repo, number: Number(n), title: pr.title, body: pr.body ?? '', author: pr.user.login, headSha, headRef: pr.head.ref, baseRef: pr.base.ref, linkedIssues };
      return res.end(JSON.stringify({ ...record, checks: checks.map((c: any) => ({ name: c.name, status: c.status, conclusion: c.conclusion })) }));
    }
    if (req.url === '/reviews') {
      appendFileSync(out, `${raw}\n`);
      return res.writeHead(201).end('{"url":"(benchmark: not posted)"}');
    }
    res.writeHead(404).end('{}');
  });
}
