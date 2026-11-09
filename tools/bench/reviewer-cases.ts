// Builds github.review cases for packages/eval/src/replay-judge.ts from catalog exports: the review as sealed, the pull request
// as its checkout_pr saw it, and the repo at the reviewed head. A recording keeps no checkout, so the judge's inputs are inline.
import { execFileSync, spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, symlinkSync, writeFileSync } from 'node:fs';
import { readFile } from 'node:fs/promises';
import { isAbsolute, join } from 'node:path';

const USAGE = `usage: node tools/bench/reviewer-cases.ts <inputs|evidence> <tree> <lab-dir> <number:version>...
  <lab-dir>/exports/<number>.json is \`quack export github:<owner>/<repo>#<number>\`; the cases go to <lab-dir>/cases.json.
  <tree> is the checkout whose reviewInputs builds them; evidence mode needs a tree still judged by reviewEvidence (rubric v6).`;

type Check = { name: string; status: string; conclusion: string | null };

/** The pull request record as checkout_pr printed it: title, base, description, CI checks and linked issues. */
export function parseCheckout(text: string) {
  const [, owner, repo, number, title] = /^(\S+?)\/(\S+?)#(\d+) by [^:]*: (.*)$/m.exec(text)!;
  const baseRef = /diff against origin\/(\S+?):/.exec(text)![1];
  const body = text.slice(text.indexOf('\n\nDescription:\n') + 15, text.indexOf('\n\nCI checks: '));
  const ci = /\n\nCI checks: (.*)/.exec(text)![1];
  const checks: Check[] = ci === 'none reported' ? [] : ci.split(', ').map((s) => ({ name: s.split('=')[0], status: 'completed', conclusion: s.split('=')[1] }));
  const issues = text.slice(text.indexOf(`CI checks: ${ci}`) + 11 + ci.length, text.indexOf('\n\nChanged files:\n'));
  const linkedIssues = issues.split(/\n\nLinked issue #/).slice(1).map((s) => {
    const m = /^(\d+): ([^\n]*)\n?([\s\S]*)$/.exec(s)!;
    return { number: Number(m[1]), title: m[2], body: m[3] };
  });
  return { owner, repo, number: Number(number), title, body: body === '(none)' ? '' : body, baseRef, checks, linkedIssues };
}

type Entry = { kind: string; model: Array<{ toolName?: string; timestamp: number; content: Array<{ text?: string }> }> };

// The review version and the last checkout of its head before it was sealed.
function recorded(lab: string, number: string, version: number) {
  const exp = JSON.parse(readFileSync(join(lab, 'exports', `${number}.json`), 'utf8'));
  const art = exp.artifacts.find((a: { kind: string; version: number }) => a.kind === 'github.review' && a.version === version);
  if (!art) throw new Error(`#${number} has no github.review v${version}`);
  const checkouts = (exp.transcript as Entry[]).filter((t) => t.kind === 'pi.tool-result' && t.model[0].toolName === 'checkout_pr').map((t) => ({ at: t.model[0].timestamp, text: t.model[0].content.map((c) => c.text ?? '').join('') }));
  const seen = checkouts.filter((t) => t.at <= art.sealedAt && t.text.includes(`Checked out ${art.basis}`)).at(-1);
  if (!seen) throw new Error(`#${number} v${version}: no checkout of ${art.basis} before it was sealed`);
  return { art, pr: { ...parseCheckout(seen.text), headSha: art.basis as string, state: 'open', author: null, headRef: '' } };
}

const git = (cwd: string, args: string[]) => execFileSync('git', args, { cwd, encoding: 'utf8', maxBuffer: 1 << 30 }).trim();

// A worktree of one blobless clone per repo at the reviewed head; the tree's node_modules stand in for the installed dependencies.
function treeAt(lab: string, tree: string, name: string, pr: { owner: string; repo: string; headSha: string; baseRef: string }) {
  const clone = join(lab, 'clones', `${pr.owner}-${pr.repo}`);
  if (!existsSync(clone)) execFileSync('git', ['clone', '--filter=blob:none', '--no-checkout', `https://github.com/${pr.owner}/${pr.repo}.git`, clone]);
  git(clone, ['fetch', '--filter=blob:none', 'origin', 'main']);
  // A squash-merged trunk keeps the merge base, so the trunk's tip diffs the head as its base did then.
  git(clone, ['update-ref', `refs/remotes/origin/${pr.baseRef}`, 'origin/main']);
  const dir = join(lab, 'trees', name, 'repo');
  if (existsSync(dir)) return dir;
  mkdirSync(join(lab, 'trees', name), { recursive: true });
  if (spawnSync('git', ['cat-file', '-e', `${pr.headSha}^{commit}`], { cwd: clone }).status !== 0) git(clone, ['fetch', '--filter=blob:none', 'origin', pr.headSha]);
  git(clone, ['worktree', 'add', '--detach', dir, pr.headSha]);
  symlinkSync(join(tree, 'node_modules'), join(dir, 'node_modules'));
  return dir;
}

// The live judge's api as far as the inputs read it: the PR doc, and commands and file reads in the checkout.
function apiFor(pr: unknown) {
  const exec = async (argv: string[], o: { onOutput?: (t: string) => void; timeout?: number } = {}) => {
    const r = spawnSync(argv[0], argv.slice(1), { encoding: 'utf8', maxBuffer: 1 << 30, timeout: (o.timeout ?? 120) * 1000 });
    if (r.error) return { ok: false, error: { code: (r.error as { code?: string }).code === 'ETIMEDOUT' ? 'timeout' : 'error', message: r.error.message } };
    o.onOutput?.(r.stdout + r.stderr);
    return { ok: true, value: { exitCode: r.status } };
  };
  const readTextFile = async (path: string) => readFile(isAbsolute(path) ? path : join(process.cwd(), path), 'utf8').then((value) => ({ ok: true, value }), () => ({ ok: false }));
  return { conversationId: 'c', snapshot: async () => ({ pr }), env: { exec, readTextFile } };
}

async function main([mode, tree, lab, ...specs]: string[]) {
  if (!['inputs', 'evidence'].includes(mode) || !tree || !lab || !specs.length) throw new Error(USAGE);
  const review = await import(join(tree, 'packages/github/src/review.ts'));
  const file = join(lab, 'cases.json');
  const cases: Array<Record<string, unknown>> = existsSync(file) ? JSON.parse(readFileSync(file, 'utf8')) : [];
  for (const spec of specs) {
    const [number, v] = spec.split(':');
    const { art, pr } = recorded(lab, number, Number(v));
    const name = `${number}-v${v}`;
    const dir = treeAt(lab, tree, name, pr);
    const sealed = { kind: 'github.review', version: Number(v), content: art.content };
    const c = cases.find((x) => x.case === name) ?? cases[cases.push({ case: name, kind: 'github.review', content: art.content }) - 1];
    if (mode === 'inputs') c.inputs = await review.reviewInputs(dir)(apiFor(pr), sealed, {});
    else c.evidence = await review.reviewEvidence(dir)(apiFor(pr), sealed, {});
    console.log(name, art.outcome, art.verdict?.passed ? 'passed' : 'failed', `against ${pr.baseRef}`);
  }
  writeFileSync(file, JSON.stringify(cases, null, 1));
}

if (import.meta.main) await main(process.argv.slice(2));
