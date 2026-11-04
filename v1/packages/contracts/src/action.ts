import { randomUUID } from 'node:crypto';
import { obj, str } from '@earendil-works/pi-schema';
import { submitToSession, type Action } from '@quack/sdk';

const params = obj({ description: str({ minLength: 1, description: 'What the contract must do and for whom, in the person\'s words' }) }, { open: true });

/** Starts the contracts agent on a description in a session of its own. */
export const defineContract: Action<typeof params> = {
  name: 'define_contract',
  summary: 'Define a contract (a REST endpoint, or a JSON Schema for a config file or document) from a description: quack asks the open questions, then writes a versioned spec',
  params,
  async start({ description }, ctx) {
    const id = randomUUID();
    const session = `contracts:${id}`;
    const title = draftTitle(description);
    const res = await submitToSession(ctx.distributorUrl, { key: session, agent: 'contracts', title, requestId: `home-contract:${id}`, input: description }, ctx.fetch);
    if (!res.ok) throw new Error(`the distributor refused the contract: HTTP ${res.status}`);
    return { session, title };
  },
};

// A working title from the description's first clause, cut at a word; the spec's own title takes over once it exists.
export function draftTitle(description: string): string {
  const first = description.replace(/\s+/g, ' ').trim().split(/(?<=[^\s]):\s|\.\s|\s\(/)[0]!;
  return first.length <= 60 ? first : `${first.slice(0, 60).replace(/\s+\S*$/, '')}…`;
}
