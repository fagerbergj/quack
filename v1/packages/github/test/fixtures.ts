import { execFileSync } from 'node:child_process';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';

const dirs: string[] = [];
const servers: Server[] = [];
export async function cleanup() {
  servers.forEach((s) => s.close());
  await Promise.all(dirs.map((d) => rm(d, { recursive: true, force: true })));
}
export const tmp = async () => { const d = await mkdtemp(join(tmpdir(), 'quack-gh-')); dirs.push(d); return d; };
const gitEnv = { ...process.env, GIT_AUTHOR_NAME: 't', GIT_AUTHOR_EMAIL: 't@t', GIT_COMMITTER_NAME: 't', GIT_COMMITTER_EMAIL: 't@t' };
export const sh = (cwd: string, ...args: string[]) => execFileSync('git', args, { cwd, encoding: 'utf8', env: gitEnv }).trim();

const DEFAULT_BASE = { 'AGENTS.md': '# Conventions\nComments stay at two lines.\n', 'app.ts': 'export const a = 1;\n' };
const DEFAULT_HEAD = { 'app.ts': 'export const a = 2;\n' };

/** A local "github.com": bare acme/widgets.git with main and refs/pull/7/head; by default the PR changes app.ts line 1. */
export async function remotes(base: Record<string, string> = DEFAULT_BASE, change: Record<string, string> = DEFAULT_HEAD) {
  const root = await tmp();
  const work = await tmp();
  const write = async (files: Record<string, string>) => {
    for (const [path, text] of Object.entries(files)) {
      await mkdir(dirname(join(work, path)), { recursive: true });
      await writeFile(join(work, path), text);
    }
  };
  sh(work, 'init', '-q', '-b', 'main');
  await write(base);
  sh(work, 'add', '.');
  sh(work, 'commit', '-qm', 'base');
  await write(change);
  sh(work, 'commit', '-qam', 'change');
  const head = sh(work, 'rev-parse', 'HEAD');
  sh(root, 'init', '-q', '--bare', 'acme/widgets.git');
  sh(work, 'push', '-q', join(root, 'acme/widgets.git'), 'HEAD~1:refs/heads/main', `${head}:refs/pull/7/head`);
  return { base: `file://${root}`, head };
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
