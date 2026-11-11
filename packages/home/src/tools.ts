import { contentText, validateToolArguments } from '@earendil-works/pi-ai';
import { int, lit, map, obj, opt, str, union, unknown } from '@earendil-works/pi-schema';
import { defineTool, type ToolRegistration } from '@earendil-works/pi-durable';
import { escapeTerm, gated, type Action, type ActionContext } from '@quack/sdk';
import { type Distributor, type SessionExport, type SessionRow } from './client.ts';

const STATES = ['running', 'needs_you', 'failed', 'done'] as const;
type State = (typeof STATES)[number];
const MAX_ARTIFACTS = 3;
const MAX_MESSAGES = 6;
const MAX_READ_CHARS = 4000;
const MAX_BODY_CHARS = 6000;

const text = (t: string) => ({ output: [{ type: 'text' as const, text: t }] });
const failure = (t: string) => ({ output: [{ type: 'text' as const, text: t }], isError: true });
const clip = (s: string, n: number) => (s.length > n ? `${s.slice(0, n)}…` : s);
const oneLine = (s: string) => s.replace(/\s+/g, ' ').trim();
/** A relative link the web app resolves under any prefix. */
export const sessionLink = (key: string) => `#/s/${encodeURIComponent(key)}`;

// The row carries all of it: a pending ask needs a person, a failed last run is failed, a run going is running.
const stateOf = (row: SessionRow): State =>
  row.pendingAsks > 0 ? 'needs_you' : row.lastRun?.outcome === 'failed' && row.state !== 'running' ? 'failed' : row.state === 'running' ? 'running' : 'done';

const line = (row: SessionRow, state: State) => `${row.title || row.key} | key ${row.key} | ${row.agent} | ${state} | ${row.lastActivity} | ${sessionLink(row.key)}`;

async function known(d: Distributor, key: string): Promise<SessionRow | string> {
  if (key.startsWith('home:')) return 'home conversations are not sessions to act on';
  return (await d.session(key)) ?? `no session with key ${key}; use find_sessions to get the key`;
}

// Each part of a session read is best effort: one failing endpoint should not hide the rest.
const safely = async <T>(fn: () => Promise<T>, empty: T): Promise<T> => fn().catch(() => empty);

function artifactLines(newest: Awaited<ReturnType<Distributor['artifacts']>>): string[] {
  if (!newest.length) return [];
  return ['Newest results:', ...newest.map((a) => {
    const delivered = a.deliveries?.length ? ` (${a.deliveries.map((d) => `${d.where} ${d.link}`).join('; ')})` : '';
    return `- ${a.kind} v${a.version}${a.summary ? `: ${oneLine(a.summary)}` : ''}${delivered}`;
  })];
}

// The newest shown version of each kind, in full up to MAX_BODY_CHARS; the cut says where the rest is.
function bodyLines(key: string, shown: Awaited<ReturnType<Distributor['artifacts']>>, sealed: SessionExport['artifacts']): string[] {
  return shown.filter((a, i) => shown.findIndex((b) => b.kind === a.kind) === i).flatMap((a) => {
    const body = sealed.find((s) => s.kind === a.kind && s.version === a.version)?.content;
    if (body === undefined) return [];
    const more = body.length - MAX_BODY_CHARS;
    return [`--- ${a.kind} v${a.version} ---`, more > 0 ? `${body.slice(0, MAX_BODY_CHARS)}\n… [${more} more characters; the whole ${a.kind} is at ${sessionLink(key)}]` : body];
  });
}

// The view the session's own GET /messages gives: the newest person and assistant entries with content, oldest first.
const lastMessages = (transcript: SessionExport['transcript']) => transcript
  .filter((e) => (e.kind === 'pi.user' || e.kind === 'pi.assistant') && e.model[0]?.content)
  .slice(-MAX_MESSAGES)
  .map((e) => ({ role: e.kind === 'pi.user' ? 'user' : 'assistant', text: contentText(e.model[0]!.content!) }));

const askLine = (a: Awaited<ReturnType<Distributor['pendingAsks']>>[number]) =>
  a.type === 'question' && a.prompt ? `- ${oneLine(a.prompt.question)} [${a.prompt.options.join(' / ')}]` : `- approve ${a.toolCall?.tool ?? 'a tool call'}?`;

export async function readSession(d: Distributor, row: SessionRow): Promise<string> {
  // The export, not GET /messages, so a read never starts a stopped session's process.
  const [artifacts, asks, exported] = await Promise.all([safely(() => d.artifacts(row.key), []), safely(() => d.pendingAsks(row.key), []), safely(() => d.exported(row.key), { transcript: [], artifacts: [] })]);
  const [shown, messages] = [artifacts.slice(0, MAX_ARTIFACTS), lastMessages(exported.transcript)];
  const failures = row.lastRun?.outcome === 'failed' ? row.lastRun.failedSteps : [];
  const out = [`${row.title || row.key} (${row.agent}), state ${stateOf(row)}`, `link ${sessionLink(row.key)}`];
  out.push(...artifactLines(shown));
  if (asks.length) out.push('Waiting on you:', ...asks.map(askLine));
  if (failures.length) out.push('Failures:', ...failures.map((f) => `- ${f.step}: ${clip(oneLine(f.error), 200)}`));
  if (messages.length) out.push('Last messages:', ...messages.map((m) => `- ${m.role}: ${clip(oneLine(m.text), 400)}`));
  return [clip(out.join('\n'), MAX_READ_CHARS), ...bodyLines(row.key, shown, exported.artifacts)].join('\n');
}

const keyParam = str({ description: 'Session key from find_sessions' });

/** The `start` tool: asks the person first (the gate shows the action's summary and the filled params), then calls the action. */
function startTool(actions: Action<any>[], ctx: ActionContext): ToolRegistration {
  const menu = actions.map((a) => `- ${a.name}: ${a.summary}. Params ${JSON.stringify(a.params)}`).join('\n');
  const parameters = obj({ action: str({ description: 'Action name' }), params: map(unknown(), { description: 'The action params' }) }, { open: true });
  const inner = gated(defineTool({
    name: 'start',
    description: 'start',
    parameters: obj({ action: str(), summary: str(), params: map(unknown()) }, { open: true }),
    async execute(args) {
      const action = actions.find((a) => a.name === args.action)!;
      const started = await action.start(args.params as never, ctx);
      return text(`Started ${started.title}. Session key ${started.session}, link ${sessionLink(started.session)}`);
    },
  }) as ToolRegistration);
  return defineTool({
    name: 'start',
    description: `Start new work. The person is asked to confirm before anything starts, so call it once the params are filled.\n${menu}`,
    parameters,
    replay: 'safe',
    async execute(args, api, c) {
      const action = actions.find((a) => a.name === args.action);
      if (!action) return failure(`unknown action ${args.action}; use one of ${actions.map((a) => a.name).join(', ')}`);
      let params: unknown;
      try {
        params = validateToolArguments({ name: action.name, description: action.summary, parameters: action.params }, { type: 'toolCall', id: action.name, name: action.name, arguments: args.params as never });
      } catch (err) {
        return failure(`bad params for ${action.name}: ${err instanceof Error ? err.message : String(err)}`);
      }
      // The gate records these args, so the approval row shows what will run.
      return inner.execute({ action: action.name, summary: `${action.summary}: ${Object.entries(params as object).map(([k, v]) => `${k} ${String(v)}`).join(', ')}`, params } as never, api, c);
    },
  }) as ToolRegistration;
}

export function homeTools(d: Distributor, actions: Action<any>[], ctx: ActionContext): ToolRegistration[] {
  const tools = [
    defineTool({
      name: 'find_sessions',
      description: 'Find sessions, newest first. Filter by words in the title or key, by agent, or by state: running, needs_you, failed, done.',
      parameters: obj({ text: opt(str()), agent: opt(str()), state: opt(union(STATES.map((s) => lit(s)))), limit: opt(int({ minimum: 1, maximum: 25 })) }, { open: true }),
      replay: 'safe',
      async execute(args) {
        const q = [`-key:${escapeTerm('home:')}*`, ...(args.agent ? [`agent:${escapeTerm(args.agent)}`] : [])].join(' AND ');
        const rows = await d.sessions(q);
        const needle = args.text?.toLowerCase();
        const found = rows
          .filter((r) => !needle || `${r.key} ${r.title ?? ''} ${r.agent}`.toLowerCase().includes(needle))
          .map((r) => ({ r, state: stateOf(r) }))
          .filter((x) => !args.state || x.state === args.state)
          .sort((a, b) => b.r.lastActivity.localeCompare(a.r.lastActivity))
          .slice(0, args.limit ?? 10);
        return text(found.length ? found.map((x) => line(x.r, x.state)).join('\n') : 'No sessions match.');
      },
    }),
    defineTool({
      name: 'read_session',
      description: 'Read one session: its state, newest results with their summaries and full text, open questions, failures and the last messages. Read it before answering anything about it.',
      parameters: obj({ key: keyParam }, { open: true }),
      replay: 'safe',
      async execute(args) {
        const row = await known(d, args.key);
        return typeof row === 'string' ? failure(row) : text(await readSession(d, row));
      },
    }),
    defineTool({
      name: 'send_message',
      description: 'Add a message to an existing session. It runs after the session finishes its current step.',
      parameters: obj({ key: keyParam, message: str({ minLength: 1 }) }, { open: true }),
      async execute(args, api) {
        const row = await known(d, args.key);
        if (typeof row === 'string') return failure(row);
        await d.send(row.key, row.agent, args.message, `home:${String(api.taskId)}`);
        return text(`Sent to ${row.title || row.key}, link ${sessionLink(row.key)}`);
      },
    }),
    startTool(actions, ctx),
    defineTool({
      name: 'stop_session',
      description: 'Abort the run in a session the person named.',
      parameters: obj({ key: keyParam }, { open: true }),
      async execute(args) {
        const row = await known(d, args.key);
        if (typeof row === 'string') return failure(row);
        await d.abort(row.key);
        return text(`Stopped ${row.title || row.key}`);
      },
    }),
    defineTool({
      name: 'run_again',
      description: 'Rerun a failed session from its failed step.',
      parameters: obj({ key: keyParam }, { open: true }),
      async execute(args) {
        const row = await known(d, args.key);
        if (typeof row === 'string') return failure(row);
        await d.resume(row.key);
        return text(`Rerunning ${row.title || row.key}, link ${sessionLink(row.key)}`);
      },
    }),
  ];
  return tools as unknown as ToolRegistration[];
}
