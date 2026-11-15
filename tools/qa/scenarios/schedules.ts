import { eachView, visible, type Scenario } from '../scenario.ts';

const scenario: Scenario = {
  name: 'schedules',
  stack: { apps: ['schedules'] },
  async run(ctx) {
    await eachView(ctx, 'schedules page', async (page, label) => {
      await page.goto(`${ctx.url}/#/schedules`);
      await visible(page.locator('main').getByText('daily-digest').first());
      await visible(page.locator('main').getByText('Every Monday at 9:00 America/New_York'));
      await ctx.shot(page, `schedules-${label}`);
    });
    await ctx.step('deleting a schedule through the gateway', async () => {
      const context = await ctx.browser.newContext();
      try {
        const page = await context.newPage();
        await page.goto(`${ctx.url}/#/schedules`);
        await page.getByRole('button', { name: 'Delete weekly-retro' }).click();
        await page.locator('main').getByText('weekly-retro', { exact: true }).waitFor({ state: 'hidden', timeout: 10_000 });
        const list = (await (await fetch(`${ctx.url}/apps/schedules`)).json()) as { schedules: Array<{ id: string; source?: string }> };
        if (list.schedules.map((s) => s.id).join() !== 'daily-digest' || 'source' in list.schedules[0]!) throw new Error(`unexpected list ${JSON.stringify(list)}`);
        const put = await fetch(`${ctx.url}/apps/schedules/daily-digest`, { method: 'PUT', body: '{}' });
        if (put.status !== 401) throw new Error(`a credential-free PUT answered ${put.status}, not the gateway's 401`);
      } finally {
        await context.close();
      }
    });
  },
};
export default scenario;
