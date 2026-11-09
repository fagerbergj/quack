import { createHmac } from 'node:crypto';
import { eachView, visible, type Scenario } from '../scenario.ts';
import { QA_WEBHOOK_SECRET } from '../stack.ts';

const KEY = 'github:fagerbergj/quack#2246';
const repository = { full_name: 'fagerbergj/quack', name: 'quack', owner: { login: 'fagerbergj' } };
let delivery = 0;

// A signed pull_request webhook, as GitHub sends it; the github app writes it to its mirror and streams it.
async function hook(url: string, action: 'reopened' | 'closed') {
  const merged = action === 'closed';
  const at = merged ? '2026-11-09T12:00:00Z' : null;
  const pull_request = { number: 2246, state: merged ? 'closed' : 'open', merged, merged_at: at, closed_at: at, html_url: 'https://github.com/fagerbergj/quack/pull/2246', title: 'feat(github): live PR badges' };
  const body = JSON.stringify({ action, repository, pull_request });
  const signature = `sha256=${createHmac('sha256', QA_WEBHOOK_SECRET).update(body).digest('hex')}`;
  const res = await fetch(`${url}/apps/github/webhook`, { method: 'POST', body, headers: { 'x-github-event': 'pull_request', 'x-hub-signature-256': signature, 'x-github-delivery': `qa-${++delivery}` } });
  if (!res.ok) throw new Error(`webhook ${action}: ${res.status} ${await res.text()}`);
}

const scenario: Scenario = {
  name: 'pr-live',
  stack: { seed: 'demo', apps: ['github'] },
  async run(ctx) {
    await ctx.step('seed a review session', async () => {
      const res = await fetch(`${ctx.url}/sessions/${encodeURIComponent(KEY)}/invocations?agent=quack`, { method: 'POST', body: JSON.stringify({ input: 'Review PR 2246', wait: true }) });
      if (!res.ok) throw new Error(`seed: ${res.status} ${await res.text()}`);
    });
    await eachView(ctx, 'badge flips on the webhook', async (page, label) => {
      await hook(ctx.url, 'reopened');
      const reads: string[] = [];
      page.on('request', (r) => r.url().includes('/apps/github/repos/') && reads.push(r.url()));
      await page.goto(`${ctx.url}/#/sessions`);
      const row = page.getByRole('link', { name: /#2246/ }).first();
      await visible(row.getByText('Open', { exact: true }));
      await ctx.shot(page, `pr-live-open-${label}`);
      const before = reads.length;
      await hook(ctx.url, 'closed');
      await visible(row.getByText('Merged', { exact: true }));
      if (reads.length !== before) throw new Error('the badge changed by a re-read, not the event');
      await ctx.shot(page, `pr-live-merged-${label}`);
    });
  },
};
export default scenario;
