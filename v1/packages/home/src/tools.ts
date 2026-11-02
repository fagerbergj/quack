import { Type, validateToolArguments, type TSchema } from '@earendil-works/pi-ai';
import { defineTool, type ToolRegistration } from '@earendil-works/pi-durable';
import { gated, type Action, type ActionContext } from '@quack/sdk';
import { type Distributor, type SessionRow, type Standing } from './client.ts';

const STATES = ['running', 'needs_you', 'failed', 'done'] as const;
type State = (typeof STATES)[number];
const MAX_ARTIFACTS = 3;
const MAX_MESSAGES = 6;
const MAX_READ_CHARS = 4000;

const text = (t: string) => ({ output: [{ type: 'text' as const, text: t }] });
const failure = (t: string) => ({ output: [{ type: 'text' as const, text: t }], isError: true });
const clip = (s: string, n: number) => (s.length > n ? `${s.slice(0, n)}…` : s);
const oneLine = (s: string) => s.replace(/\s+/g, ' ').trim();
/** A relative link the web app resolves under any prefix. */
export const sessionLink = (key: string) => `#/s/${encodeURIComponent(key)}`;

const stateOf = (row: SessionRow, standing: Standing): State =>
  standing.needsYou.has(row.key) ? 'needs_you' : standing.failed.has(row.key) ? 'failed' : row.active ? 'running' : 'done';

const line = (row: SessionRow, state: State) => `${row.title || row.key} | key ${row.key} | ${row.agent} | ${state} | ${row.lastActivity} | ${sessionLink(row.key)}`;

async function known(d: Distributor, key: string): Promise<SessionRow | string> {
  if (key.startsWith('home:')) return 'home conversations are not sessions to act on';
  return (await d.sessions()).find((s) => s.key === key) ?? `no session with key ${key}; use find_sessions to get the key`;
}

// Each part of a session read is best effort: one failing endpoint should not hide the rest.
const safely = async <T>(fn: () => Promise<T>, empty: T): Promise<T> => fn().catch(() => empty);

export async function readSession(d: Distributor, row: SessionRow, standing: Standing): Promise<string> {
  const [ping, artifacts, questions, failures, messages] = await Promise.all([
    safely(() => d.ping(row.key), undefined), safely(() => d.artifacts(row.key), []), safely(() => d.questions(row.key), []),
    safely(() => d.failures(row.key), []), safely(() => d.messages(row.key, MAX_MESSAGES), []),
  ]);
  const state = ping?.waitingOnHuman || questions.length ? 'needs_you' : failures.length ? 'failed' : ping?.status === 'HealthyBusy' ? 'running' : stateOf(row, standing);
  const out = [`${row.title || row.key} (${row.agent}), state ${state}${ping?.note ? `, ${oneLine(ping.note)}` : ''}`, `link ${sessionLink(row.key)}`];
  const newest = artifacts.slice(-MAX_ARTIFACTS).reverse();
  if (newest.length) out.push('Newest results:');
  for (const a of newest) {
    const verdict = a.verdict ? (a.verdict.passed ? ', checks passed' : ', checks failed') : '';
    out.push(`- ${a.kind} v${a.version}${verdict}: ${a.summary ? oneLine(a.summary) : clip(oneLine(a.content), 300)}${a.links?.length ? ` (${a.links.map((l) => `${l.label} ${l.url}`).join('; ')})` : ''}`);
  }
  if (questions.length) out.push('Open questions:', ...questions.map((q) => `- ${oneLine(q.question)} [${q.options.join(' / ')}]`));
  if (failures.length) out.push('Failures:', ...failures.map((f) => `- ${f.step}: ${clip(oneLine(f.error), 200)}`));
  if (messages.length) out.push('Last messages:', ...messages.map((m) => `- ${m.role}: ${clip(oneLine(m.text), 400)}`));
  return clip(out.join('\n'), MAX_READ_CHARS);
}

const keyParam = Type.String({ description: 'Session key from find_sessions' });

/** The `start` tool: asks the person first (the gate shows the action's summary and the filled params), then calls the action. */
function startTool(actions: Action<any>[], ctx: ActionContext): ToolRegistration<TSchema> {
  const menu = actions.map((a) => `- ${a.name}: ${a.summary}. Params ${JSON.stringify(a.params)}`).join('\n');
  const parameters = Type.Object({ action: Type.String({ description: 'Action name' }), params: Type.Record(Type.String(), Type.Any(), { description: 'The action params' }) });
  const inner = gated(defineTool({
    name: 'start',
    description: 'start',
    parameters: Type.Object({ action: Type.String(), summary: Type.String(), params: Type.Record(Type.String(), Type.Any()) }),
    async execute(args) {
      const action = actions.find((a) => a.name === args.action)!;
      const started = await action.start(args.params as never, ctx);
      return text(`Started ${started.title}. Session key ${started.session}, link ${sessionLink(started.session)}`);
    },
  }) as ToolRegistration<TSchema>);
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
      return inner.execute({ action: action.name, summary: action.summary, params } as never, api, c);
    },
  }) as ToolRegistration<TSchema>;
}

export function homeTools(d: Distributor, actions: Action<any>[], ctx: ActionContext): ToolRegistration<TSchema>[] {
  const tools = [
    defineTool({
      name: 'find_sessions',
      description: 'Find sessions, newest first. Filter by words in the title or key, by agent, or by state: running, needs_you, failed, done.',
      parameters: Type.Object({ text: Type.Optional(Type.String()), agent: Type.Optional(Type.String()), state: Type.Optional(Type.Union(STATES.map((s) => Type.Literal(s)))), limit: Type.Optional(Type.Integer({ minimum: 1, maximum: 25 })) }),
      replay: 'safe',
      async execute(args) {
        const [rows, standing] = await Promise.all([d.sessions(), d.standing()]);
        const needle = args.text?.toLowerCase();
        const found = rows
          .filter((r) => !r.key.startsWith('home:') && (!args.agent || r.agent === args.agent) && (!needle || `${r.key} ${r.title ?? ''} ${r.agent}`.toLowerCase().includes(needle)))
          .map((r) => ({ r, state: stateOf(r, standing) }))
          .filter((x) => !args.state || x.state === args.state)
          .sort((a, b) => b.r.lastActivity.localeCompare(a.r.lastActivity))
          .slice(0, args.limit ?? 10);
        return text(found.length ? found.map((x) => line(x.r, x.state)).join('\n') : 'No sessions match.');
      },
    }),
    defineTool({
      name: 'read_session',
      description: 'Read one session: its state, newest results with their summaries, open questions, failures and the last messages. Read it before answering anything about it.',
      parameters: Type.Object({ key: keyParam }),
      replay: 'safe',
      async execute(args) {
        const row = await known(d, args.key);
        return typeof row === 'string' ? failure(row) : text(await readSession(d, row, await d.standing()));
      },
    }),
    defineTool({
      name: 'send_message',
      description: 'Add a message to an existing session. It runs after the session finishes its current step.',
      parameters: Type.Object({ key: keyParam, message: Type.String({ minLength: 1 }) }),
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
      parameters: Type.Object({ key: keyParam }),
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
      parameters: Type.Object({ key: keyParam }),
      async execute(args) {
        const row = await known(d, args.key);
        if (typeof row === 'string') return failure(row);
        await d.resume(row.key);
        return text(`Rerunning ${row.title || row.key}, link ${sessionLink(row.key)}`);
      },
    }),
  ];
  return tools as unknown as ToolRegistration<TSchema>[];
}
