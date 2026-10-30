import { execFileSync } from 'node:child_process';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const dirs: string[] = [];
const servers: Server[] = [];
export async function cleanup() {
  servers.forEach((s) => s.close());
  await Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true })));
}
export const tmp = async () => { const d = await mkdtemp(join(tmpdir(), 'quack-gh-')); dirs.push(d); return d; };
const gitEnv = { ...process.env, GIT_AUTHOR_NAME: 't', GIT_AUTHOR_EMAIL: 't@t', GIT_COMMITTER_NAME: 't', GIT_COMMITTER_EMAIL: 't@t' };
export const sh = (cwd: string, ...args: string[]) => execFileSync('git', args, { cwd, encoding: 'utf8', env: gitEnv }).trim();

/** A local "github.com": bare acme/widgets.git with main and refs/pull/7/head, where the PR changes app.ts line 1. */
export async function remotes() {
  const base = await tmp();
  const work = await tmp();
  sh(work, 'init', '-q', '-b', 'main');
  await writeFile(join(work, 'AGENTS.md'), '# Conventions\nComments stay at two lines.\n');
  await writeFile(join(work, 'app.ts'), 'export const a = 1;\n');
  sh(work, 'add', '.');
  sh(work, 'commit', '-qm', 'base');
  await writeFile(join(work, 'app.ts'), 'export const a = 2;\n');
  sh(work, 'commit', '-qam', 'change');
  const head = sh(work, 'rev-parse', 'HEAD');
  sh(base, 'init', '-q', '--bare', 'acme/widgets.git');
  sh(work, 'push', '-q', join(base, 'acme/widgets.git'), 'HEAD~1:refs/heads/main', `${head}:refs/pull/7/head`);
  return { base: `file://${base}`, head };
}

export async function listen(server: Server) {
  servers.push(server.listen(0));
  await new Promise((r) => server.once('listening', r));
  return `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
}

/** The service half's session-facing API, faked: PR metadata, credentials, and posted reviews recorded. */
export async function fakeService(head: string) {
  const posted: any[] = [];
  const url = await listen(
    createServer(async (req, res) => {
      if (req.headers.authorization !== 'Bearer svc') return res.writeHead(401).end('{}');
      let raw = '';
      for await (const c of req) raw += c;
      if (req.url?.startsWith('/git-credential')) return res.writeHead(200).end(JSON.stringify({ username: 'x-access-token', token: 'ghs_from_service' }));
      if (req.url === '/reviews') {
        posted.push(JSON.parse(raw));
        return res.writeHead(201).end(JSON.stringify({ id: 9, url: 'https://github.com/acme/widgets/pull/7#review-9' }));
      }
      const pr = { owner: 'acme', repo: 'widgets', number: 7, title: 'Bump a', body: 'Fixes #3', author: 'dev', headSha: head, headRef: 'bump', baseRef: 'main', checks: [{ name: 'ci', status: 'completed', conclusion: 'success' }], linkedIssues: [{ number: 3, title: 'a should be 2', body: 'It is 1.' }] };
      res.writeHead(200).end(JSON.stringify(pr));
    }),
  );
  return { url, posted };
}
