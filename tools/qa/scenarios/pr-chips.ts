import { eachView, prWebhook, seedReview, visible, type Scenario } from '../scenario.ts';

// Open, merged and closed come from signed webhooks to the github app; acme/widgets has no installation, so its badge stays neutral.
const PRS = [
  { key: 'github:fagerbergj/quack#2241', name: 'Pull request #2241, open, on GitHub', state: 'open' },
  { key: 'github:fagerbergj/quack#2243', name: 'Pull request #2243, merged, on GitHub', state: 'merged' },
  { key: 'github:fagerbergj/quack#2242', name: 'Pull request #2242, closed, on GitHub', state: 'closed' },
  { key: 'github:acme/widgets#7', name: 'Pull request #7 on GitHub', state: 'unknown' },
] as const;
const NOT_INSTALLED = { status: 404, contentType: 'application/problem+json', body: JSON.stringify({ status: 404, detail: 'the app is not installed on acme/widgets' }) };

const scenario: Scenario = {
  name: 'pr-chips',
  stack: { seed: 'demo', apps: ['github'] },
  async run(ctx) {
    await ctx.step('seed four review sessions and their PR states', async () => {
      for (const pr of PRS) await seedReview(ctx.url, pr.key);
      for (const pr of PRS) if (pr.state !== 'unknown') await prWebhook(ctx.url, 'fagerbergj/quack', Number(pr.key.split('#')[1]), pr.state);
    });
    await eachView(ctx, 'sessions list', async (page, label) => {
      // The app would ask GitHub for acme/widgets with its throwaway key; the rig answers as an uninstalled repo does.
      await page.route('**/apps/github/repos/acme/widgets/pulls/7', (r) => r.fulfill(NOT_INSTALLED));
      await page.context().route('https://github.com/**', (r) => r.abort());
      await page.goto(`${ctx.url}/#/sessions`);
      for (const pr of PRS) await visible(page.getByRole('link', { name: pr.name }).first());
      const merged = page.getByRole('link', { name: PRS[1].name }).first();
      if ((await merged.getAttribute('href')) !== 'https://github.com/fagerbergj/quack/pull/2243') throw new Error('the badge links elsewhere');
      await ctx.shot(page, `pr-chips-${label}`);
      await merged.focus();
      await ctx.shot(page, `pr-chips-focus-${label}`);
      const [tab] = await Promise.all([page.waitForEvent('popup'), merged.click()]);
      await tab.close();
      if (!page.url().endsWith('#/sessions')) throw new Error(`the badge click also opened the session: ${page.url()}`);
    });
    await eachView(ctx, 'session header', async (page, label) => {
      await page.route('**/apps/github/repos/acme/widgets/pulls/7', (r) => r.fulfill(NOT_INSTALLED));
      for (const pr of PRS) {
        await page.goto(`${ctx.url}/#/s/${encodeURIComponent(pr.key)}`);
        await visible(page.locator('main').getByRole('link', { name: pr.name }));
        await ctx.shot(page, `pr-chips-header-${pr.state}-${label}`);
      }
    });
  },
};
export default scenario;
