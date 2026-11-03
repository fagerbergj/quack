import { randomUUID } from 'node:crypto';
import { Type } from '@earendil-works/pi-ai';
import { submitToSession, type Action } from '@quack/sdk';

const params = Type.Object({ description: Type.String({ minLength: 1, description: 'What the contract must do and for whom, in the person\'s words' }) });

/** Starts the contracts agent on a description in a session of its own. */
export const defineContract: Action<typeof params> = {
  name: 'define_contract',
  summary: 'Define a contract (a REST endpoint) from a description: quack asks the open questions, then writes a versioned spec',
  params,
  async start({ description }, ctx) {
    const id = randomUUID();
    const session = `contracts:${id}`;
    const title = description.replace(/\s+/g, ' ').trim().slice(0, 80);
    const res = await submitToSession(ctx.distributorUrl, { key: session, agent: 'contracts', title, requestId: `home-contract:${id}`, input: description }, ctx.fetch);
    if (!res.ok) throw new Error(`the distributor refused the contract: HTTP ${res.status}`);
    return { session, title };
  },
};
