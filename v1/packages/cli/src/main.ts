#!/usr/bin/env node
import { writeFile } from 'node:fs/promises';
import { parseArgs } from 'node:util';
import { ApiError, client, type Client } from './api.ts';

type Out = { stdout: (s: string) => void; stderr: (s: string) => void };
type Opts = { state?: string; agent?: string; version?: string; json?: boolean; url?: string; help?: boolean };
// A command returns the API's JSON and how to print it for a person.
type Result = { data: unknown; text: string };
type Command = { usage: string; run: (c: Client, pos: string[], o: Opts) => Promise<Result> };

const STATES = ['running', 'needs_you', 'failed', 'done'];
const usageError = (m: string) => new ApiError(m, -1);
const ok = (data: unknown): Result => ({ data, text: 'ok' });

function table(rows: Array<Array<string | number | undefined>>, head: string[]): string {
  const all = [head, ...rows.map((r) => r.map((v) => String(v ?? '')))];
  const widths = head.map((_, i) => Math.max(...all.map((r) => r[i]!.length)));
  return all.map((r) => r.map((v, i) => v.padEnd(widths[i]!)).join('  ').trimEnd()).join('\n');
}

const when = (ms: number) => new Date(ms).toISOString();

// A key the distributor does not list is not found; asking it for one would start a session.
async function known(c: Client, key: string): Promise<any> {
  const s = (await c.sessions()).find((x) => x.key === key);
  if (!s) throw new ApiError(`no such session: ${key}`, 404);
  return s;
}

// Mirrors the UI's groups: a session with an approval, question or failure needs you, whatever else it is doing.
async function filterByState(c: Client, sessions: any[], state: string): Promise<any[]> {
  const [approvals, questions, failures] = await Promise.all([c.index('approvals', '?state=pending'), c.index('questions', '?state=pending'), c.index('failures')]);
  const keys = (rows: any[]) => new Set(rows.map((r) => r.session));
  const failed = keys(failures);
  const asks = new Set([...keys(approvals), ...keys(questions), ...failed]);
  const test: Record<string, (s: any) => boolean> = {
    failed: (s) => failed.has(s.key),
    needs_you: (s) => asks.has(s.key),
    running: (s) => s.active && !asks.has(s.key),
    done: (s) => !s.active && !asks.has(s.key),
  };
  return sessions.filter(test[state]!);
}

async function sessions(c: Client, _: string[], o: Opts): Promise<Result> {
  if (o.state && !STATES.includes(o.state)) throw usageError(`--state must be one of ${STATES.join(', ')}`);
  let list = (await c.sessions()).filter((s) => !s.archived && (!o.agent || s.agent === o.agent));
  if (o.state) list = await filterByState(c, list, o.state);
  return { data: { sessions: list }, text: table(list.map((s) => [s.key, s.agent, s.running ? (s.active ? 'running' : 'idle') : 'stopped', s.title, s.lastActivity]), ['KEY', 'AGENT', 'STATE', 'TITLE', 'LAST ACTIVITY']) };
}

const textOf = (content: unknown): string =>
  typeof content === 'string' ? content : Array.isArray(content) ? content.filter((b) => b?.type === 'text').map((b) => b.text).join('\n') : '';

const lastMessages = (snap: Awaited<ReturnType<Client['view']>>, n = 5) =>
  snap.items.flatMap((i) => i.entry?.model ?? []).filter((m) => m.role !== 'toolResult' && textOf(m.content)).slice(-n).map((m) => ({ role: m.role, text: textOf(m.content) }));

async function show(c: Client, [key]: string[]): Promise<Result> {
  if (!key) throw usageError('usage: quack show <key>');
  const session = await known(c, key);
  const [artifacts, questions, approvals, snap] = await Promise.all([c.artifacts(key), c.questions(key), c.approvals(key), c.view(key)]);
  const messages = lastMessages(snap);
  const newest = [...new Map(artifacts.map((a) => [a.kind, a])).values()];
  const lines = [
    `${session.key}  ${session.agent}  ${session.running ? (session.active ? 'running' : 'idle') : 'stopped'}${session.title ? `  ${session.title}` : ''}`,
    '', 'Artifacts', table(newest.map((a) => [a.kind, `v${a.version}`, a.verdict ? (a.verdict.passed ? 'passed' : 'failed') : '', a.summary]), ['KIND', 'VERSION', 'VERDICT', 'SUMMARY']),
    '', 'Questions', table(questions.map((q) => [q.id, q.question]), ['ID', 'QUESTION']),
    '', 'Approvals', table(approvals.map((a) => [a.id, a.tool]), ['ID', 'TOOL']),
    '', 'Last messages', ...messages.map((m) => `${m.role}: ${m.text}`),
  ];
  return { data: { session, artifacts, questions, approvals, messages }, text: lines.join('\n') };
}

async function approvals(c: Client): Promise<Result> {
  const list = await c.index('approvals', '?state=pending');
  return { data: { approvals: list }, text: table(list.map((a) => [a.session, a.id, a.tool, when(a.at)]), ['SESSION', 'ID', 'TOOL', 'ASKED']) };
}

async function artifacts(c: Client, [key]: string[]): Promise<Result> {
  const list = key ? (await known(c, key), (await c.artifacts(key)).map((a) => ({ session: key, ...a }))) : await c.index('artifacts');
  return { data: { artifacts: list }, text: table(list.map((a) => [a.session, a.kind, `v${a.version}`, a.summary]), ['SESSION', 'KIND', 'VERSION', 'SUMMARY']) };
}

async function artifact(c: Client, [key, kind]: string[], o: Opts): Promise<Result> {
  if (!key || !kind) throw usageError('usage: quack artifact <key> <kind> [--version N]');
  await known(c, key);
  const of = (await c.artifacts(key)).filter((a) => a.kind === kind);
  const found = o.version ? of.find((a) => String(a.version) === o.version) : of.reduce((m, a) => (!m || a.version > m.version ? a : m), undefined);
  if (!found) throw new ApiError(`no ${kind} artifact${o.version ? ` version ${o.version}` : ''} in ${key}`, 404);
  return { data: found, text: found.content };
}

const need = (n: number, usage: string) => (pos: string[]) => { if (pos.length < n) throw usageError(`usage: quack ${usage}`); };

// Wraps a one-key action: checks the arguments and, for ones that would start a session, that it exists.
const action = (usage: string, n: number, check: boolean, run: (c: Client, pos: string[]) => Promise<unknown>): Command => ({
  usage,
  run: async (c, pos) => {
    need(n, usage)(pos);
    if (check) await known(c, pos[0]!);
    return ok(await run(c, pos));
  },
});

// Writes the whole session as one JSON file; the default name is the key made safe for a file system.
async function exportSession(c: Client, [key, file]: string[]): Promise<Result> {
  if (!key) throw usageError('usage: quack export <key> [file]');
  await known(c, key);
  const path = file ?? `quack-${key.replace(/[^A-Za-z0-9._-]+/g, '-')}.json`;
  const body = await c.exported(key);
  await writeFile(path, JSON.stringify(body, null, 2));
  return { data: body, text: `wrote ${path}` };
}

const COMMANDS: Record<string, Command> = {
  sessions: { usage: 'sessions [--state running|needs_you|failed|done] [--agent A]', run: sessions },
  show: { usage: 'show <key>', run: show },
  send: action('send <key> <text>', 2, true, (c, [k, ...t]) => c.send(k!, t.join(' '))),
  stop: action('stop <key>', 1, true, (c, [k]) => c.abort(k!)),
  resume: action('resume <key>', 1, true, (c, [k]) => c.resume(k!)),
  rename: action('rename <key> <title>', 2, false, (c, [k, ...t]) => c.rename(k!, t.join(' '))),
  archive: action('archive <key>', 1, false, (c, [k]) => c.archive(k!)),
  delete: action('delete <key>', 1, false, (c, [k]) => c.remove(k!)),
  export: { usage: 'export <key> [file]', run: exportSession },
  approvals: { usage: 'approvals', run: approvals },
  approve: action('approve <key> <id>', 2, true, (c, [k, id]) => c.decide(k!, id!, 'approve')),
  reject: action('reject <key> <id> [reason]', 2, true, (c, [k, id, ...r]) => c.decide(k!, id!, 'reject', r.join(' ') || undefined)),
  artifacts: { usage: 'artifacts [<key>]', run: artifacts },
  artifact: { usage: 'artifact <key> <kind> [--version N]', run: artifact },
  agents: { usage: 'agents', run: async (c) => { const list = await c.agents(); return { data: { agents: list }, text: table(list.map((a) => [a.name, a.version]), ['NAME', 'VERSION']) }; } },
};

const help = () => ['usage: quack [--url URL] [--json] <command>', '', ...Object.values(COMMANDS).map((c) => `  quack ${c.usage}`), '', 'URL defaults to $QUACK_URL, else http://localhost:8080. $QUACK_ADMIN_TOKEN is sent when set.'].join('\n');

/** Exit codes: 0 ok, 1 API or usage error, 2 not found. */
export async function run(argv: string[], env: Record<string, string | undefined>, out: Out): Promise<number> {
  try {
    const { values, positionals } = parseArgs({ args: argv, allowPositionals: true, options: { state: { type: 'string' }, agent: { type: 'string' }, version: { type: 'string' }, json: { type: 'boolean' }, url: { type: 'string' }, help: { type: 'boolean', short: 'h' } } });
    const [name, ...pos] = positionals;
    const command = name ? COMMANDS[name] : undefined;
    if (values.help || !name) {
      out.stdout(help());
      return 0;
    }
    if (!command) throw usageError(`unknown command ${name}; try --help`);
    const result = await command.run(client(values.url ?? env.QUACK_URL ?? 'http://localhost:8080', env.QUACK_ADMIN_TOKEN), pos, values);
    out.stdout(values.json ? JSON.stringify(result.data, null, 2) : result.text);
    return 0;
  } catch (e) {
    out.stderr(`quack: ${e instanceof Error ? e.message : String(e)}`);
    return e instanceof ApiError && e.status === 404 ? 2 : 1;
  }
}

if (import.meta.main) process.exitCode = await run(process.argv.slice(2), process.env, { stdout: (s) => console.log(s), stderr: (s) => console.error(s) });
