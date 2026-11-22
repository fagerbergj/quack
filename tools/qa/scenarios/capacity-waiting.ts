import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { eachView, visible, type Scenario } from '../scenario.ts';

const A = 'github:qa/waiting#1';
const B = 'github:qa/waiting#2';
const HOG = 'qa-hog';
let dataDir = '';

// Holds the one `local` slot as a lease row for ten minutes (the ledger reads the file on every call), so the sessions below queue behind it.
const ledger = (fn: (db: DatabaseSync) => void) => {
  const db = new DatabaseSync(join(dataDir, 'capacity.db'));
  try {
    fn(db);
  } finally {
    db.close();
  }
};
const holdSlot = () => ledger((db) => db.prepare("INSERT OR REPLACE INTO entries (endpoint, holder, state, since, expires) VALUES ('local', ?, 'held', ?, ?)").run(HOG, Date.now(), Date.now() + 600_000));
const freeSlot = () => ledger((db) => db.prepare("DELETE FROM entries WHERE endpoint = 'local' AND holder = ?").run(HOG));

const start = async (url: string, key: string) => {
  const res = await fetch(`${url}/sessions/${encodeURIComponent(key)}/invocations?agent=quack`, { method: 'POST', body: JSON.stringify({ input: `Review ${key}`, wait: false }) });
  if (!res.ok) throw new Error(`start ${key}: ${res.status} ${await res.text()}`);
};

// The distributor reads each process's wait on its sweep (every 30 s), so the list row follows within that.
const rowsWaiting = async (url: string) => {
  const { sessions } = (await (await fetch(`${url}/sessions`)).json()) as { sessions: Array<{ key: string; waiting?: { ahead: number; provider: string } }> };
  return Object.fromEntries(sessions.map((s) => [s.key, s.waiting?.provider === 'local' ? s.waiting.ahead : undefined]));
};

const scenario: Scenario = {
  name: 'capacity-waiting',
  stack: { script: () => ({ content: 'Done.' }), apps: ['capacity'], capacity: 1 },
  async run(ctx) {
    dataDir = ctx.dataDir;
    await ctx.step('two sessions queue behind a held slot and the Sessions API rows carry their place', async () => {
      holdSlot();
      await start(ctx.url, A);
      await new Promise((r) => setTimeout(r, 500));
      await start(ctx.url, B);
      const end = Date.now() + 60_000;
      let seen: Record<string, number | undefined> = {};
      while (Date.now() < end) {
        seen = await rowsWaiting(ctx.url);
        if (seen[A] === 0 && seen[B] === 1) return;
        await new Promise((r) => setTimeout(r, 1000));
      }
      throw new Error(`rows waiting: ${JSON.stringify(seen)}`);
    });
    // Each view takes its screenshot before asserting, so a run against an older web build still shows what the page said.
    await eachView(ctx, 'the session view says it waits for a slot', async (page, label) => {
      await page.goto(`${ctx.url}/#/s/${encodeURIComponent(B)}`);
      const said = await visible(page.getByText('Waiting…').first()).then(() => true, () => false);
      await ctx.shot(page, `waiting-session-${label}`);
      if (!said) throw new Error('no waiting line in the session view');
    });
    await eachView(ctx, 'the sessions list marks both as waiting', async (page, label) => {
      await page.goto(`${ctx.url}/#/sessions`);
      const row = page.getByRole('link', { name: /qa\/waiting#2|Review github:qa\/waiting#2/ }).first();
      const said = await visible(row.getByText('Waiting…')).then(() => true, () => false);
      await ctx.shot(page, `waiting-list-${label}`);
      if (!said) throw new Error('no waiting line in the list row');
    });
    await ctx.step('freeing the slot lets both run to the end and the wait clears', async () => {
      freeSlot();
      const end = Date.now() + 60_000;
      while (Date.now() < end) {
        const { sessions } = (await (await fetch(`${ctx.url}/sessions`)).json()) as { sessions: Array<{ key: string; state: string; waiting?: unknown }> };
        if (sessions.length >= 2 && sessions.every((s) => s.state === 'idle' && !s.waiting)) return;
        await new Promise((r) => setTimeout(r, 1000));
      }
      throw new Error('sessions did not finish');
    });
  },
};
export default scenario;
