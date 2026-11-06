import { randomUUID } from 'node:crypto';
import { obj, str } from '@earendil-works/pi-schema';
import { submitToSession, type Action } from '@quack/sdk';

const params = obj({ question: str({ minLength: 1, description: 'The question to research, with any URLs it names' }) }, { open: true });

/** Starts the researcher on a question in a session of its own. */
export const researchQuestion: Action<typeof params> = {
  name: 'research_question',
  summary: 'Answer a question with a sourced research report',
  params,
  async start({ question }, ctx) {
    const id = randomUUID();
    const session = `research:${id}`;
    const title = question.replace(/\s+/g, ' ').trim().slice(0, 80);
    const res = await submitToSession(ctx.distributorUrl, { key: session, agent: 'researcher', title, requestId: `home-research:${id}`, input: question }, ctx.fetch);
    if (!res.ok) throw new Error(`the distributor refused the question: HTTP ${res.status}`);
    return { session, title };
  },
};
