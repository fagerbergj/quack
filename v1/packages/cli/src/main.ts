#!/usr/bin/env node
import { spawn } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { writeFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';
import { ApiError, client, type Client } from './api.ts';

type Out = { stdout: (s: string) => void; stderr: (s: string) => void };
type Opts = { state?: string; agent?: string; version?: string; json?: boolean; url?: string; help?: boolean; print?: boolean; follow?: boolean; keys?: boolean };
// A command returns the API's JSON and how to print it for a person.
type Result = { data: unknown; text: string; streamed?: boolean };
type Command = { usage: string; run: (c: Client, pos: string[], o: Opts, out: Out) => Promise<Result> };

const STATES = ['running', 'needs_you', 'failed', 'done'];
const usageError = (m: string) => new ApiError(m, -1);
const ok = (data: unknown): Result => ({ data, text: 'ok' });

function table(rows: Array<Array<string | number | undefined>>, head: string[]): string {
  const all = [head, ...rows.map((r) => r.map((v) => String(v ?? '')))];
  const widths = head.map((_, i) => Math.max(...all.map((r) => r[i]!.length)));
  return all.map((r) => r.map((v, i) => v.padEnd(widths[i]!)).join('  ').trimEnd()).join('\n');
}

const when = (ms: number) => new Date(ms).toISOString();

// A key the distributor does not list is not found; a read of one answers 404, so check first for a clear message.
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
  if (o.keys) return { data: { sessions: list }, text: list.map((s) => s.key).join('\n') };
  return { data: { sessions: list }, text: table(list.map((s) => [s.key, s.agent, s.running ? (s.active ? 'running' : 'idle') : 'stopped', s.title, s.lastActivity]), ['KEY', 'AGENT', 'STATE', 'TITLE', 'LAST ACTIVITY']) };
}

const textOf = (content: unknown): string =>
  typeof content === 'string' ? content : Array.isArray(content) ? content.filter((b) => b?.type === 'text').map((b) => b.text).join('\n') : '';

const messagesOf = (items: Array<{ entry?: { model?: Array<{ role: string; content: unknown }> } }>) =>
  items.flatMap((i) => i.entry?.model ?? []).filter((m) => m.role !== 'toolResult' && textOf(m.content)).map((m) => ({ role: m.role, text: textOf(m.content) }));

async function show(c: Client, [key]: string[], o: Opts, out: Out): Promise<Result> {
  if (!key) throw usageError('usage: quack show <key>');
  const session = await known(c, key);
  const [artifacts, questions, approvals, snap] = await Promise.all([c.artifacts(key), c.questions(key), c.approvals(key), c.view(key)]);
  const all = messagesOf(snap.items);
  const messages = all.slice(-5);
  const newest = [...new Map(artifacts.map((a) => [a.kind, a])).values()];
  const lines = [
    `${session.key}  ${session.agent}  ${session.running ? (session.active ? 'running' : 'idle') : 'stopped'}${session.title ? `  ${session.title}` : ''}`,
    '', 'Artifacts', table(newest.map((a) => [a.kind, `v${a.version}`, a.verdict ? (a.verdict.passed ? 'passed' : 'failed') : '', a.summary]), ['KIND', 'VERSION', 'VERDICT', 'SUMMARY']),
    '', 'Questions', table(questions.map((q) => [q.id, q.question]), ['ID', 'QUESTION']),
    '', 'Approvals', table(approvals.map((a) => [a.id, a.tool]), ['ID', 'TOOL']),
    '', 'Last messages', ...messages.map((m) => `${m.role}: ${m.text}`),
  ];
  const data = { session, artifacts, questions, approvals, messages };
  if (!o.follow) return { data, text: lines.join('\n') };
  out.stdout(lines.join('\n'));
  // shortcut: counts messages in the newest window, so a window that slides mid-run can repeat or skip a line; fine for tailing.
  let seen = all.length;
  await c.follow(key, (items) => {
    const now = messagesOf(items ?? []);
    if (items) for (const m of now.slice(seen)) out.stdout(`${m.role}: ${m.text}`);
    if (items) seen = Math.max(seen, now.length);
  });
  return { data, text: '', streamed: true };
}

// With -p the run is waited for and only the answer is printed, so the output pipes; a run that does not finish exits 1.
async function started(c: Client, key: string, text: string, o: Opts, agent?: string): Promise<Result> {
  const r = await c.invoke(key, text, { agent, wait: o.print });
  if (!o.print) return { data: { key, ...r }, text: key };
  if (r.status !== 'done') throw new ApiError(`run ${r.status}${r.reason ? `: ${r.reason}` : ''}${r.detail ? ` (${r.detail})` : ''}`, -1);
  return { data: { key, ...r }, text: r.answer ?? '' };
}

async function newSession(c: Client, pos: string[], o: Opts, out: Out): Promise<Result> {
  if (!pos.length) throw usageError('usage: quack new [--agent A] [-p] <text>');
  const key = `ui:${randomUUID()}`;
  if (o.print) out.stderr(`session ${key}`);
  return started(c, key, pos.join(' '), o, o.agent);
}

async function send(c: Client, [key, ...text]: string[], o: Opts): Promise<Result> {
  if (!key || !text.length) throw usageError('usage: quack send [-p] <key> <text>');
  await known(c, key);
  if (o.print) return started(c, key, text.join(' '), o);
  return ok(await c.send(key, text.join(' ')));
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
  sessions: { usage: 'sessions [--state running|needs_you|failed|done] [--agent A] [--keys]', run: sessions },
  show: { usage: 'show [-f] <key>', run: show },
  new: { usage: 'new [--agent A] [-p] <text>', run: newSession },
  send: { usage: 'send [-p] <key> <text>', run: send },
  stop: action('stop <key>', 1, true, (c, [k]) => c.abort(k!)),
  resume: action('resume <key>', 1, true, (c, [k]) => c.resume(k!)),
  rename: action('rename <key> <title>', 2, false, (c, [k, ...t]) => c.rename(k!, t.join(' '))),
  archive: action('archive <key>', 1, false, (c, [k]) => c.archive(k!)),
  unarchive: action('unarchive <key>', 1, false, (c, [k]) => c.unarchive(k!)),
  delete: action('delete <key>', 1, false, (c, [k]) => c.remove(k!)),
  export: { usage: 'export <key> [file]', run: exportSession },
  approvals: { usage: 'approvals', run: approvals },
  approve: action('approve <key> <id>', 2, true, (c, [k, id]) => c.decide(k!, id!, 'approve')),
  reject: action('reject <key> <id> [reason]', 2, true, (c, [k, id, ...r]) => c.decide(k!, id!, 'reject', r.join(' ') || undefined)),
  artifacts: { usage: 'artifacts [<key>]', run: artifacts },
  artifact: { usage: 'artifact <key> <kind> [--version N]', run: artifact },
  completion: { usage: 'completion bash|zsh', run: async (_c, pos) => completion(pos) },
  eval: { usage: 'eval <args>', run: async () => { throw usageError('eval takes its own arguments: quack eval <command> ...'); } },
  agents: { usage: 'agents', run: async (c) => { const list = await c.agents(); return { data: { agents: list }, text: table(list.map((a) => [a.name, a.version]), ['NAME', 'VERSION']) }; } },
};

// Static: the subcommand list and, after a key-taking command, the live keys from `quack sessions --keys`.
function completion([shell]: string[]): Result {
  if (shell !== 'bash' && shell !== 'zsh') throw usageError('usage: quack completion bash|zsh');
  const names = Object.keys(COMMANDS).join(' ');
  const keyed = Object.entries(COMMANDS).filter(([, c]) => c.usage.includes('<key>')).map(([n]) => n).join('|');
  const script = [
    ...(shell === 'zsh' ? ['autoload -U +X bashcompinit && bashcompinit'] : []),
    '_quack() {',
    `  if [ "$COMP_CWORD" -eq 1 ]; then COMPREPLY=($(compgen -W "${names}" -- "$2"))`,
    `  else case "\${COMP_WORDS[1]}" in ${keyed}) COMPREPLY=($(compgen -W "$(quack sessions --keys 2>/dev/null)" -- "$2"));; esac; fi`,
    '}',
    'complete -F _quack quack',
  ].join('\n');
  return { data: { shell, script }, text: script };
}

const EVAL = fileURLToPath(new URL('../../eval/src/main.ts', import.meta.url));
// Eval has its own flags, so it gets the raw arguments before this parser sees them.
const evalPassthrough = (args: string[], env: Record<string, string | undefined>) =>
  new Promise<number>((resolve) => spawn(process.execPath, [EVAL, ...args], { stdio: 'inherit', env }).on('close', (code) => resolve(code ?? 1)));

const help = () => ['usage: quack [--url URL] [--json] <command>', '', ...Object.values(COMMANDS).map((c) => `  quack ${c.usage}`), '', 'URL defaults to $QUACK_URL, else http://localhost:8080. $QUACK_ADMIN_TOKEN is sent when set.'].join('\n');

/** Exit codes: 0 ok, 1 API or usage error, 2 not found. */
export async function run(argv: string[], env: Record<string, string | undefined>, out: Out): Promise<number> {
  if (argv[0] === 'eval') return evalPassthrough(argv.slice(1), env);
  try {
    const { values, positionals } = parseArgs({ args: argv, allowPositionals: true, options: { state: { type: 'string' }, agent: { type: 'string' }, version: { type: 'string' }, json: { type: 'boolean' }, url: { type: 'string' }, help: { type: 'boolean', short: 'h' }, print: { type: 'boolean', short: 'p' }, follow: { type: 'boolean', short: 'f' }, keys: { type: 'boolean' } } });
    const [name, ...pos] = positionals;
    const command = name ? COMMANDS[name] : undefined;
    if (values.help || !name) {
      out.stdout(help());
      return 0;
    }
    if (!command) throw usageError(`unknown command ${name}; try --help`);
    const result = await command.run(client(values.url ?? env.QUACK_URL ?? 'http://localhost:8080', env.QUACK_ADMIN_TOKEN), pos, values, out);
    if (!result.streamed) out.stdout(values.json ? JSON.stringify(result.data, null, 2) : result.text);
    return 0;
  } catch (e) {
    out.stderr(`quack: ${e instanceof Error ? e.message : String(e)}`);
    return e instanceof ApiError && e.status === 404 ? 2 : 1;
  }
}

if (import.meta.main) process.exitCode = await run(process.argv.slice(2), process.env, { stdout: (s) => console.log(s), stderr: (s) => console.error(s) });
