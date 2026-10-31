// The GitHub service half for benchmarks: PR metadata through gh at a pinned head, no credentials, reviews recorded and never posted.
import { execFileSync } from 'node:child_process';
import { appendFileSync } from 'node:fs';
import { createServer } from 'node:http';

const gh = (path: string) => JSON.parse(execFileSync('gh', ['api', path], { encoding: 'utf8', maxBuffer: 64 << 20 }));
const CLOSES = /\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)/gi;

/** pins maps "owner/repo#n" to the head sha to review; out is where posted reviews are appended. */
export function stubService(pins: Record<string, string>, out: string) {
  return createServer(async (req, res) => {
    let raw = '';
    for await (const c of req) raw += c;
    const m = /^\/pulls\/([^/]+)\/([^/]+)\/(\d+)$/.exec(req.url ?? '');
    if (m) {
      const [, owner, repo, n] = m;
      const pr = gh(`/repos/${owner}/${repo}/pulls/${n}`);
      const headSha = pins[`${owner}/${repo}#${n}`] ?? pr.head.sha;
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
