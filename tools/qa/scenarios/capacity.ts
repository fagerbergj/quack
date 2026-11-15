import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import type { FakeScript } from '../fake-model.ts';
import type { Scenario } from '../scenario.ts';

// A key with ':', '/' and '#', which the gate must percent-encode as one path segment.
const KEY = 'github:qa/capacity#1';
let dataDir = '';
const during: string[][] = [];

const heldOnLocal = () => {
  const db = new DatabaseSync(join(dataDir, 'capacity.db'), { readOnly: true });
  try {
    return (db.prepare("SELECT holder FROM entries WHERE endpoint = 'local' AND state = 'held' ORDER BY seq").all() as Array<{ holder: string }>).map((r) => r.holder);
  } finally {
    db.close();
  }
};

// The fake model looks at the capacity store while a chat call is in flight, so the check sees the gate's lease, not a later state.
// Requests without messages are the provider's model listing, which no lease covers.
const script: FakeScript = (r) => (r.messages && during.push(heldOnLocal()), { content: 'Done.' });

const scenario: Scenario = {
  name: 'capacity',
  stack: { script, apps: ['capacity'], capacity: 1 },
  async run(ctx) {
    dataDir = ctx.dataDir;
    await ctx.step('a session holds the local lease through /apps/capacity while its model call runs', async () => {
      const res = await fetch(`${ctx.url}/sessions/${encodeURIComponent(KEY)}/invocations?agent=quack`, { method: 'POST', body: JSON.stringify({ input: 'Say done.', wait: true }) });
      if (!res.ok) throw new Error(`invoke: ${res.status} ${await res.text()}`);
      if (JSON.stringify(during) !== JSON.stringify([[KEY]])) throw new Error(`held during the call: ${JSON.stringify(during)}`);
    });
    await ctx.step('the lease goes back when the run ends', async () => {
      for (let i = 0; i < 40 && heldOnLocal().length; i++) await new Promise((r) => setTimeout(r, 250));
      if (heldOnLocal().length) throw new Error(`still held: ${JSON.stringify(heldOnLocal())}`);
    });
    await ctx.step('the gateway serves capacity only with a credential, and slots no longer', async () => {
      const capacity = await fetch(`${ctx.url}/apps/capacity/endpoints`);
      const slots = await fetch(`${ctx.url}/apps/slots/`);
      if (capacity.status !== 401 || slots.status !== 404) throw new Error(`/apps/capacity answered ${capacity.status}, /apps/slots ${slots.status}`);
    });
  },
};
export default scenario;
