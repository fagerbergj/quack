import { eachView, visible, type Scenario } from '../scenario.ts';

const scenario: Scenario = {
  name: 'schedules',
  stack: { apps: { schedules: 'stub' } },
  async run(ctx) {
    await eachView(ctx, 'schedules page', async (page, label) => {
      await page.goto(`${ctx.url}/#/schedules`);
      await visible(page.locator('main').getByText('daily-digest').first());
      await visible(page.locator('main').getByText('Every Monday at 9:00 America/New_York'));
      await ctx.shot(page, `schedules-${label}`);
    });
  },
};
export default scenario;
