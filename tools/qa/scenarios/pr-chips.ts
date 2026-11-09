import { eachView, visible, type Scenario } from '../scenario.ts';

const MERGED = 'github:fagerbergj/quack#2243';
const UNKNOWN = 'github:fagerbergj/quack#2241';

const scenario: Scenario = {
  name: 'pr-chips',
  stack: { seed: 'demo' },
  async run(ctx) {
    await ctx.step('seed two review sessions', async () => {
      for (const key of [MERGED, UNKNOWN]) {
        const res = await fetch(`${ctx.url}/sessions/${encodeURIComponent(key)}/invocations?agent=quack`, { method: 'POST', body: JSON.stringify({ input: `Review PR ${key.split('#')[1]}`, wait: true }) });
        if (!res.ok) throw new Error(`seed ${key}: ${res.status} ${await res.text()}`);
      }
    });
    await eachView(ctx, 'sessions list', async (page, label) => {
      // The github service needs a GitHub App, so the rig answers its lookup: #2243 merged, #2241 unknown.
      await page.route('**/apps/github/pulls?*', (r) => r.fulfill({ json: { pulls: { 'fagerbergj/quack#2243': { state: 'merged', at: '2026-11-09T02:00:00Z' } } } }));
      await page.context().route('https://github.com/**', (r) => r.abort());
      await page.goto(`${ctx.url}/#/sessions`);
      const merged = page.getByRole('link', { name: 'Pull request #2243, merged, on GitHub' }).first();
      await visible(merged);
      await visible(page.getByRole('link', { name: '#2241 on GitHub' }).first());
      if ((await merged.getAttribute('href')) !== 'https://github.com/fagerbergj/quack/pull/2243') throw new Error('chip links elsewhere');
      await ctx.shot(page, `pr-chips-${label}`);
      await merged.focus();
      await ctx.shot(page, `pr-chips-focus-${label}`);
      const [tab] = await Promise.all([page.waitForEvent('popup'), merged.click()]);
      await tab.close();
      if (!page.url().endsWith('#/sessions')) throw new Error(`the chip click also opened the session: ${page.url()}`);
    });
    await eachView(ctx, 'session header', async (page, label) => {
      await page.route('**/apps/github/pulls?*', (r) => r.fulfill({ json: { pulls: { 'fagerbergj/quack#2243': { state: 'merged', at: '2026-11-09T02:00:00Z' } } } }));
      await page.goto(`${ctx.url}/#/s/${encodeURIComponent(MERGED)}`);
      const chip = page.locator('main').getByRole('link', { name: 'Pull request #2243, merged, on GitHub' });
      await visible(chip);
      await chip.focus();
      await ctx.shot(page, `pr-chips-header-${label}`);
    });
  },
};
export default scenario;
