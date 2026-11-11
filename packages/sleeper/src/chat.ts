import { obj, opt, str } from '@earendil-works/pi-schema';
import { defineTool, type ToolRegistration } from '@earendil-works/pi-durable';
import { Artifacts } from '@quack/sdk';
import { latestRun, type Chat, type ChatSession, type Workflow } from '@quack/workflows';
import type { SleeperClient } from './client.ts';
import { sessionClient, sessionNews } from './deps.ts';
import type { NewsItem } from './lineup/facts.ts';
import type { Searchable } from './lineup/news.ts';

export const CHAT_PROMPT = `You answer follow-up questions about one Sleeper football job: its latest result and the facts it was built from.

- Read before you answer: job_result is the sealed result, job_facts what it was built from, player a player's status and news. Answer only from them; if they miss it, say so.
- Open with the answer, no preamble. Say what you used in plain words (the lineup, the player) and give news as markdown links; never versions, check names, ids or scores. Never invent news, sources, projections or totals.
- Sleeper's injury designation lags beat-writer news; when they differ, say which you followed.
- To redo the job, call rerun_job; it takes no hint, so say the request cannot change it.
- Answer in a few sentences, with the numbers you used. You cannot change a lineup or roster.`;

const MAX_CHARS = 20_000;
const reply = (value: unknown, isError = false) => {
  const text = typeof value === 'string' ? value : JSON.stringify(value);
  return { output: [{ type: 'text' as const, text: text.length > MAX_CHARS ? `${text.slice(0, MAX_CHARS)}\n... cut at ${MAX_CHARS} of ${text.length} characters` : text }], isError };
};

export type ChatDeps = { client: SleeperClient; news?: (p: Searchable, at: number) => Promise<NewsItem[]> };

/** Read-only tools over the session's job: the sealed result, the facts behind it and a player's status and news. */
export function chatTools(deps: ChatDeps, session: () => ChatSession): ToolRegistration[] {
  const result = defineTool({
    name: 'job_result',
    description: "The latest sealed version of each result this session's job produced, and the judge's verdict when it has one.",
    parameters: obj({}, { open: true }),
    async execute(_args, api, ctx) {
      const doc = await api.snapshot(Artifacts, api.conversationId, ctx);
      const latest = Object.values(doc?.kinds ?? {}).flatMap((k) => k.versions.slice(-1));
      if (!latest.length) return reply('Nothing has been sealed in this session yet.', true);
      return reply(latest.map((v) => ({ kind: v.kind, version: v.version, sealedAt: new Date(v.sealedAt).toISOString(), verdict: v.verdict, content: v.content })));
    },
  });
  const facts = defineTool({
    name: 'job_facts',
    description: "What the latest run was built from: each step's results (the facts step holds the data) and the run's status. Pass a step name to read just that step.",
    parameters: obj({ step: opt(str()) }, { open: true }),
    async execute(args, _api, ctx) {
      const run = await latestRun(session(), ctx);
      if (!run) return reply('The job has not run in this session yet.', true);
      const results = args.step ? { [args.step]: run.results[args.step] } : run.results;
      return reply({ status: run.status, input: run.input, results });
    },
  });
  const player = defineTool({
    name: 'player',
    description: "A player's current Sleeper status (team, position, injury designation, depth) and recent credible news with links. Pass the full name.",
    parameters: obj({ name: str({ minLength: 2 }) }, { open: true }),
    async execute(args, api) {
      const wanted = args.name.trim().toLowerCase();
      const dump = (await deps.client.players()) as Record<string, Record<string, string | number | null>>;
      const nameOf = (p: Record<string, string | number | null>) => String(p.full_name ?? [p.first_name, p.last_name].filter(Boolean).join(' '));
      const found = Object.entries(dump).filter(([, p]) => nameOf(p).toLowerCase() === wanted).slice(0, 3);
      if (!found.length) return reply(`No player named ${args.name}.`, true);
      return reply(await Promise.all(found.map(async ([id, p]) => {
        const news = deps.news ? await deps.news({ name: nameOf(p), team: p.team as string | null, pos: String(p.position) }, api.now()) : 'no news source is configured';
        return { id, name: nameOf(p), team: p.team, position: p.position, status: p.status, injury_status: p.injury_status, injury_body_part: p.injury_body_part, injury_notes: p.injury_notes, depth_chart_order: p.depth_chart_order, news };
      })));
    },
  });
  return [result, facts, player];
}

const chatDepsFromEnv = (env: Record<string, string | undefined> = process.env): ChatDeps => ({ client: sessionClient(), news: sessionNews(env) });

/** A sleeper job's workflow with the follow-up chat added; the Sleeper API and news come from the environment. */
export function withChat(workflow: Workflow, deps: ChatDeps = chatDepsFromEnv()): Workflow {
  return { ...workflow, chat: { prompt: CHAT_PROMPT, tools: (session) => chatTools(deps, session) } satisfies Chat };
}
