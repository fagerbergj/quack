import { eachView, prWebhook, seedReview, visible, type Scenario } from '../scenario.ts';

const KEY = 'github:fagerbergj/quack#2246';

const scenario: Scenario = {
  name: 'pr-live',
  stack: { seed: 'demo', apps: ['github'] },
  async run(ctx) {
    await ctx.step('seed a review session', () => seedReview(ctx.url, KEY));
    await eachView(ctx, 'badge flips on the webhook', async (page, label) => {
      await prWebhook(ctx.url, 'fagerbergj/quack', 2246, 'open');
      const reads: string[] = [];
      page.on('request', (r) => r.url().includes('/apps/github/repos/') && reads.push(r.url()));
      await page.goto(`${ctx.url}/#/sessions`);
      await visible(page.getByRole('link', { name: 'Pull request #2246, open, on GitHub' }).first());
      await ctx.shot(page, `pr-live-open-${label}`);
      const before = reads.length;
      await prWebhook(ctx.url, 'fagerbergj/quack', 2246, 'merged');
      await visible(page.getByRole('link', { name: 'Pull request #2246, merged, on GitHub' }).first());
      if (reads.length !== before) throw new Error('the badge changed by a re-read, not the event');
      await ctx.shot(page, `pr-live-merged-${label}`);
    });
  },
};
export default scenario;
