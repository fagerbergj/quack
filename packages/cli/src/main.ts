#!/usr/bin/env node
import { spawn } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { writeFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';
import { escapeTerm } from '@quack/sdk/query';
import { ApiError, client, whole, type Client, type Page } from './api.ts';

type Out = { stdout: (s: string) => void; stderr: (s: string) => void };
type Opts = {
  state?: string; agent?: string; key?: string; version?: string; json?: boolean; url?: string; help?: boolean; print?: boolean; follow?: boolean; keys?: boolean; verdict?: boolean;
  session?: string; text?: string; reason?: string; messages?: string; 'order-by'?: string; 'page-size'?: string; 'page-token'?: string;
};
// A command returns the API's JSON and how to print it for a person; `next` says how to read the next page, on stderr.
type Result = { data: unknown; text: string; streamed?: boolean; next?: string };
type Command = { usage: string; run: (c: Client, pos: string[], o: Opts, out: Out) => Promise<Result> };

// The UI's groups as q: a session waiting on a person needs you whatever else it is doing.
const STATES: Record<string, string> = {
  running: 'state:running',
  needs_you: 'pendingAsks:>0',
  failed: 'lastRun.outcome:failed',
  done: '(state:idle OR state:stopped) AND pendingAsks:0',
};
const usageError = (m: string) => new ApiError(m, -1);
const ok = (data: unknown): Result => ({ data, text: 'ok' });
const and = (...terms: Array<string | false | undefined>) => terms.filter(Boolean).join(' AND ') || undefined;
const listed = (data: any, text: string): Result => ({ data, text, next: data.nextPageToken && `--page-token ${data.nextPageToken}` });

type Orders = Record<string, 'asc' | 'desc'>;
// The direction each list's orders run in; the Sessions API takes the field alone, as each order runs one way.
const SESSION_ORDERS: Orders = { lastActivity: 'desc', createdAt: 'desc', oldestAskAt: 'asc' };

/** An AIP-132 `field [asc|desc]` as the field the server takes; a direction the order does not run in is refused here. */
function orderBy(value: string | undefined, orders: Orders): string | undefined {
  const [field, dir, ...extra] = value?.trim().split(/\s+/) ?? [];
  if (!field || (!dir && !extra.length)) return field || undefined;
  if (extra.length || !Object.hasOwn(orders, field) || orders[field] !== dir!.toLowerCase()) {
    throw usageError(`--order-by takes ${Object.entries(orders).map((e) => e.join(' ')).join(', ')}; the direction may be left off`);
  }
  return field;
}

const page = (o: Opts, orders: Orders, q?: string): Page => ({ q, orderBy: orderBy(o['order-by'], orders), pageSize: o['page-size'], pageToken: o['page-token'] });

function table(rows: Array<Array<string | number | undefined>>, head: string[]): string {
  const all = [head, ...rows.map((r) => r.map((v) => String(v ?? '')))];
  const widths = head.map((_, i) => Math.max(...all.map((r) => r[i]!.length)));
  return all.map((r) => r.map((v, i) => v.padEnd(widths[i]!)).join('  ').trimEnd()).join('\n');
}


// A read of a key the distributor does not know answers 404, so check first for a clear message.
async function known(c: Client, key: string): Promise<any> {
  try {
    return await c.session(key);
  } catch (e) {
    throw e instanceof ApiError && e.status === 404 ? new ApiError(`no such session: ${key}`, 404) : e;
  }
}

// A q of the caller's own replaces the default of hiding archived sessions.
async function sessions(c: Client, pos: string[], o: Opts): Promise<Result> {
  if (o.state && !Object.hasOwn(STATES, o.state)) throw usageError(`--state must be one of ${Object.keys(STATES).join(', ')}`);
  const q = and(pos.length ? `(${pos.join(' ')})` : '-state:archived', o.state && STATES[o.state], o.agent && `agent:${escapeTerm(o.agent)}`);
  const data = await c.sessions(page(o, SESSION_ORDERS, q));
  const list: any[] = data.sessions;
  if (o.keys) return listed(data, list.map((s) => s.key).join('\n'));
  return listed(data, table(list.map((s) => [s.key, s.agent, s.state, s.pendingAsks, s.title, s.lastActivity]), ['KEY', 'AGENT', 'STATE', 'ASKS', 'TITLE', 'LAST ACTIVITY']));
}

const textOf = (content: unknown): string =>
  typeof content === 'string' ? content : Array.isArray(content) ? content.filter((b) => b?.type === 'text').map((b) => b.text).join('\n') : '';

const messagesOf = (items: Array<{ entry?: { model?: Array<{ role: string; content: unknown }> } }>) =>
  items.flatMap((i) => i.entry?.model ?? []).filter((m) => m.role !== 'toolResult' && textOf(m.content)).map((m) => ({ role: m.role, text: textOf(m.content) }));

const askRow = (a: any) => [a.id, a.type, a.createdAt, a.toolCall?.tool ?? a.prompt?.question];

// The judge's rule: a gate or positive check fails when not fully met, a pitfall (negative weight) when it showed up.
const checkFailed = (c: any) => c.met !== null && (c.weight === 'gate' || c.weight > 0 ? c.met < 1 : c.met > 0);
const oneLine = (s: string, max = 120) => { const t = s.replace(/\s+/g, ' ').trim(); return t.length > max ? `${t.slice(0, max - 1)}…` : t; };

// The ledger's null is both a judge at work and a kind nobody judges, so only a verdict given is shown.
function verdictsOf(newest: any[], ledger: any[]) {
  const verdicts = newest.flatMap((a) => {
    const verdict = ledger.find((l) => l.kind === a.kind && l.version === a.version)?.verdict;
    return verdict ? [{ kind: a.kind, version: a.version, verdict }] : [];
  });
  const lines = verdicts.length ? [] : ['none'];
  lines.push(...verdicts.flatMap(({ kind, version, verdict: v }) => [
    [`${kind} v${version}`, v.passed ? 'passed' : v.limitReached ? 'failed, released at the revision limit' : 'failed', `score ${+v.score.toFixed(2)}`, v.rubric, v.gatesFailed?.length && `gates failed: ${v.gatesFailed.join(', ')}`].filter(Boolean).join('  '),
    ...v.checks.filter(checkFailed).map((c: any) => `  ${c.criterion}.${c.id}: ${oneLine(c.notes?.[0] ?? c.label ?? '')}`),
  ]));
  return { verdicts, lines };
}

async function summary(c: Client, key: string, n: number, judged?: boolean): Promise<{ data: object; lines: string[] }> {
  const session = await known(c, key);
  const [asks, artifacts, messages, ledger] = await Promise.all([
    whole((p) => c.asks(p, key), 'asks', { q: 'state:pending' }), whole((p) => c.artifacts(p, key), 'artifacts'), c.messages(key, n),
    judged ? c.ledger(key) : undefined,
  ]);
  // The list is newest first, so a kind's first row is its newest version.
  const newest = new Map<string, any>();
  for (const a of artifacts) if (!newest.has(a.kind)) newest.set(a.kind, a);
  const judgedBy = ledger && verdictsOf([...newest.values()], ledger);
  const lines = [
    `${session.key}  ${session.agent}  ${session.state}${session.title ? `  ${session.title}` : ''}`,
    '', 'Artifacts', table([...newest.values()].map((a) => [a.kind, `v${a.version}`, a.sealedAt, a.summary]), ['KIND', 'VERSION', 'SEALED', 'SUMMARY']),
    ...(judgedBy ? ['', 'Verdicts', ...judgedBy.lines] : []),
    '', 'Asks', table(asks.map(askRow), ['ID', 'TYPE', 'ASKED', 'WHAT']),
    '', 'Last messages', ...messages.map((m) => `${m.role}: ${m.text}`),
  ];
  return { data: { session, asks, artifacts, messages, ...(judgedBy && { verdicts: judgedBy.verdicts }) }, lines };
}

async function show(c: Client, [key]: string[], o: Opts, out: Out): Promise<Result> {
  if (!key) throw usageError('usage: quack show [-f] [--verdict] [--messages N] <key>');
  const n = Number(o.messages ?? 5);
  // The session server returns at most 50; refusing more beats printing fewer than asked.
  if (!Number.isInteger(n) || n < 1 || n > 50) throw usageError('--messages takes 1 to 50');
  const { data, lines } = await summary(c, key, n, o.verdict);
  if (!o.follow) return { data, text: lines.join('\n') };
  out.stdout(lines.join('\n'));
  // shortcut: the socket's first window is the baseline, so a line between the read above and it, or one a sliding window
  // drops mid-run, is not printed; fine for tailing.
  let seen: number | undefined;
  await c.follow(key, (items) => {
    if (!items) return;
    const now = messagesOf(items);
    if (seen !== undefined) for (const m of now.slice(seen)) out.stdout(`${m.role}: ${m.text}`);
    seen = Math.max(seen ?? 0, now.length);
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

// An invocation of a known key is a send, so `new --key` refuses one that exists.
async function absent(c: Client, key: string) {
  try {
    await c.session(key);
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return;
    throw e;
  }
  throw usageError(`session ${key} exists; send to it with quack send`);
}

async function newSession(c: Client, pos: string[], o: Opts, out: Out): Promise<Result> {
  if (!pos.length) throw usageError('usage: quack new [--agent A] [--key K] [-p] <text>');
  // Pages find their sessions by key prefix (Contract Studio reads contracts:*), as the web and home actions mint them.
  const key = o.key ?? `${o.agent ?? 'ui'}:${randomUUID()}`;
  if (o.key) await absent(c, key);
  if (o.print) out.stderr(`session ${key}`);
  return started(c, key, pos.join(' '), o, o.agent);
}

// A steer goes into the running turn; a plain send queues behind it as a follow-up, which withdraw takes back by this id.
async function send(c: Client, [key, ...text]: string[], o: Opts, whenBusy?: 'steer'): Promise<Result> {
  if (!key || !text.length) throw usageError(`usage: quack ${whenBusy ?? 'send [-p]'} <key> <text>`);
  await known(c, key);
  if (o.print && !whenBusy) return started(c, key, text.join(' '), o);
  const r = await c.send(key, text.join(' '), whenBusy);
  return { data: r, text: r.submissionId };
}

async function withdraw(c: Client, [key, id]: string[]): Promise<Result> {
  if (!key || !id) throw usageError('usage: quack withdraw <key> <id>');
  await known(c, key);
  if (!(await c.withdraw(key, id))) throw new ApiError(`no queued message ${id} in ${key}`, 404);
  return ok({ withdrawn: id });
}

async function asks(c: Client, pos: string[], o: Opts): Promise<Result> {
  const data = await c.asks(page(o, { createdAt: o.session ? 'desc' : 'asc' }, and('state:pending', pos.length > 0 && `(${pos.join(' ')})`)), o.session);
  return listed(data, table(data.asks.map((a: any) => [a.session, ...askRow(a)]), ['SESSION', 'ID', 'TYPE', 'ASKED', 'WHAT']));
}

async function answer(c: Client, [key, id, ...choice]: string[], o: Opts): Promise<Result> {
  if (!key || !id || !choice.length) throw usageError('usage: quack answer <key> <askId> <choice> [--text T] [--reason R]');
  return ok(await c.resolve(key, id, { choice: choice.join(' '), ...(o.text && { text: o.text }), ...(o.reason && { reason: o.reason }) }));
}

// An approval's answer with its choice filled in; a reason may also follow the id.
const decide = (choice: 'approve' | 'reject'): Command => ({
  usage: `${choice} <key> <askId> [reason]`,
  run: (c, [key = '', id = '', ...reason], o) => answer(c, [key, id, choice], { ...o, reason: o.reason ?? (reason.join(' ') || undefined) }),
});

async function artifacts(c: Client, [key]: string[], o: Opts): Promise<Result> {
  const data = await c.artifacts(page(o, { sealedAt: 'desc' }), key);
  return listed(data, table(data.artifacts.map((a: any) => [a.session, a.kind, `v${a.version}`, a.sealedAt, a.summary]), ['SESSION', 'KIND', 'VERSION', 'SEALED', 'SUMMARY']));
}

// The list is newest first, so one row is the newest version, or the one --version names.
async function artifact(c: Client, [key, kind]: string[], o: Opts): Promise<Result> {
  if (!key || !kind) throw usageError('usage: quack artifact <key> <kind> [--version N]');
  const q = and(`kind:${escapeTerm(kind)}`, o.version && `version:${escapeTerm(o.version)}`);
  const [found] = (await c.artifacts({ q, pageSize: '1' }, key)).artifacts;
  if (!found) throw new ApiError(`no ${kind} artifact${o.version ? ` version ${o.version}` : ''} in ${key}`, 404);
  return { data: found, text: found.content };
}

// A body that is one list of flat rows, and at most a page token besides, reads as a table; anything else prints as JSON.
function rowsTable(data: any): string | undefined {
  const { nextPageToken: _next, ...rest } = data && typeof data === 'object' && !Array.isArray(data) ? data : { list: data };
  const lists = Object.values(rest);
  const rows = lists.length === 1 && Array.isArray(lists[0]) ? (lists[0] as any[]) : undefined;
  if (!rows?.length || !rows.every((r) => r && typeof r === 'object' && Object.values(r).every((v) => v === null || typeof v !== 'object'))) return undefined;
  const head = [...new Set(rows.flatMap((r) => Object.keys(r)))];
  return table(rows.map((r) => head.map((h) => r[h])), head.map((h) => h.toUpperCase()));
}

// Generic over every extension app: the path is the app's own, query included, so no app needs a command of its own.
async function app(c: Client, [name, route]: string[]): Promise<Result> {
  if (!name || !route) throw usageError('usage: quack app <name> <path>');
  const data = await c.app(name, route.startsWith('/') ? route : `/${route}`);
  if (data === null) throw new ApiError(`${name} answered ${route} with no JSON`, -1);
  const next = data.nextPageToken && `add pageToken=${encodeURIComponent(data.nextPageToken)} to the path`;
  return { data, text: rowsTable(data) ?? JSON.stringify(data, null, 2), next };
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
  sessions: { usage: 'sessions [--state running|needs_you|failed|done] [--agent A] [--order-by "F [asc|desc]"] [--page-size N] [--page-token T] [--keys] [q]', run: sessions },
  show: { usage: 'show [-f] [--verdict] [--messages N] <key>', run: show },
  new: { usage: 'new [--agent A] [--key K] [-p] <text>', run: newSession },
  send: { usage: 'send [-p] <key> <text>', run: (c, pos, o) => send(c, pos, o) },
  steer: { usage: 'steer <key> <text>', run: (c, pos, o) => send(c, pos, o, 'steer') },
  withdraw: { usage: 'withdraw <key> <id>', run: withdraw },
  stop: action('stop <key>', 1, true, (c, [k]) => c.abort(k!)),
  resume: action('resume <key>', 1, true, (c, [k]) => c.resume(k!)),
  rename: action('rename <key> <title>', 2, false, (c, [k, ...t]) => c.rename(k!, t.join(' '))),
  archive: action('archive <key>', 1, false, (c, [k]) => c.archive(k!)),
  unarchive: action('unarchive <key>', 1, false, (c, [k]) => c.unarchive(k!)),
  delete: action('delete <key>', 1, false, (c, [k]) => c.remove(k!)),
  export: { usage: 'export <key> [file]', run: exportSession },
  asks: { usage: 'asks [--session K] [--page-size N] [--page-token T] [q]', run: asks },
  answer: { usage: 'answer <key> <askId> <choice> [--text T] [--reason R]', run: answer },
  approvals: { usage: 'approvals', run: (c, _pos, o) => asks(c, ['type:approval'], o) },
  approve: decide('approve'),
  reject: decide('reject'),
  artifacts: { usage: 'artifacts [--page-size N] [--page-token T] [<key>]', run: artifacts },
  artifact: { usage: 'artifact <key> <kind> [--version N]', run: artifact },
  app: { usage: 'app <name> <path>', run: app },
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

const string = { type: 'string' } as const;
const flag = { type: 'boolean' } as const;
const OPTIONS = {
  state: string, agent: string, key: string, version: string, session: string, text: string, reason: string, messages: string, 'order-by': string, 'page-size': string, 'page-token': string,
  json: flag, url: string, help: { ...flag, short: 'h' }, print: { ...flag, short: 'p' }, follow: { ...flag, short: 'f' }, keys: flag, verdict: flag,
} as const;

function print(result: Result, json: boolean | undefined, out: Out) {
  if (!result.streamed) out.stdout(json ? JSON.stringify(result.data, null, 2) : result.text);
  if (result.next && !json) out.stderr(`next page: ${result.next}`);
}

// `--order-by createdAt desc` unquoted would leave desc as a q term, so the direction joins its field.
function joinDirection(argv: string[]): string[] {
  const i = argv.indexOf('--order-by');
  return i >= 0 && /^(asc|desc)$/i.test(argv[i + 2] ?? '') ? [...argv.slice(0, i + 1), `${argv[i + 1]} ${argv[i + 2]}`, ...argv.slice(i + 3)] : argv;
}

/** Exit codes: 0 ok, 1 API or usage error, 2 not found. */
export async function run(argv: string[], env: Record<string, string | undefined>, out: Out): Promise<number> {
  if (argv[0] === 'eval') return evalPassthrough(argv.slice(1), env);
  try {
    const { values, positionals } = parseArgs({ args: joinDirection(argv), allowPositionals: true, options: OPTIONS });
    const [name, ...pos] = positionals;
    const command = name ? COMMANDS[name] : undefined;
    if (values.help || !name) {
      out.stdout(help());
      return 0;
    }
    if (!command) throw usageError(`unknown command ${name}; try --help`);
    const result = await command.run(client(values.url ?? env.QUACK_URL ?? 'http://localhost:8080', env.QUACK_ADMIN_TOKEN), pos, values, out);
    print(result, values.json, out);
    return 0;
  } catch (e) {
    out.stderr(`quack: ${e instanceof Error ? e.message : String(e)}`);
    return e instanceof ApiError && e.status === 404 ? 2 : 1;
  }
}

if (import.meta.main) process.exitCode = await run(process.argv.slice(2), process.env, { stdout: (s) => console.log(s), stderr: (s) => console.error(s) });
