import { eachView, visible, type Scenario } from '../scenario.ts';

const TITLE = 'Review PR 42: cap the retry backoff';

const scenario: Scenario = {
  name: 'home-sessions',
  stack: { seed: 'demo' },
  async run(ctx) {
    await eachView(ctx, 'home', async (page, label) => {
      await page.goto(`${ctx.url}/#/`);
      await visible(page.getByText('What do you need?'));
      await ctx.shot(page, `home-${label}`);
    });
    await eachView(ctx, 'sessions list', async (page, label) => {
      await page.goto(`${ctx.url}/#/sessions`);
      await visible(page.getByText(TITLE).first());
      await ctx.shot(page, `sessions-${label}`);
    });
    await eachView(ctx, 'session view', async (page, label) => {
      await page.goto(`${ctx.url}/#/s/qa-review`);
      // The transcript scrolls inside the live region, not the window.
      const log = page.locator('[aria-live="polite"]');
      await visible(log.getByText('Approve. The retry backoff now caps at 30 seconds'));
      await ctx.shot(page, `session-${label}`);
    });
  },
};
export default scenario;
