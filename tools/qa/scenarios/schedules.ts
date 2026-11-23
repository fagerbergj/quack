import { eachView, visible, type Scenario } from '../scenario.ts';

// No session exists for the sample keys, so each schedule is titled by its session key.
const DIGEST = 'sched:daily-digest';
const RETRO = 'sched:weekly-retro';

const scenario: Scenario = {
  name: 'schedules',
  stack: { apps: ['schedules'] },
  async run(ctx) {
    await eachView(ctx, 'schedules page', async (page, label) => {
      await page.goto(`${ctx.url}/#/schedules`);
      const main = page.locator('main');
      await visible(main.getByRole('link', { name: DIGEST }));
      await visible(main.getByText('Every Monday', { exact: true }));
      if ((await main.getByText('Every Monday', { exact: true }).getAttribute('aria-hidden')) !== 'true') throw new Error('the short cadence is read aloud beside the full one');
      await ctx.shot(page, `schedules-${label}`);
      await main.getByRole('button', { name: `More for ${RETRO}` }).click();
      await visible(main.getByRole('menu', { name: `More for ${RETRO}` }).getByRole('menuitem', { name: 'Delete schedule' }));
      await ctx.shot(page, `schedules-menu-${label}`);
    });
    await ctx.step('deleting a schedule through the gateway, after an Undo', async () => {
      const context = await ctx.browser.newContext();
      try {
        const page = await context.newPage();
        const main = page.locator('main');
        const del = () => main.getByRole('button', { name: `More for ${RETRO}` }).click().then(() => main.getByRole('menuitem', { name: 'Delete schedule' }).click());
        const deletes: string[] = [];
        page.on('request', (r) => void (r.method() === 'DELETE' && deletes.push(r.url())));
        await page.goto(`${ctx.url}/#/schedules`);
        await del();
        await main.getByRole('link', { name: RETRO }).waitFor({ state: 'hidden', timeout: 10_000 });
        await page.getByRole('status').getByRole('button', { name: 'Undo' }).click();
        await visible(main.getByRole('link', { name: RETRO }));
        const sent = page.waitForResponse((r) => r.request().method() === 'DELETE', { timeout: 15_000 });
        await del();
        await ctx.shot(page, 'schedules-undo');
        if ((await sent).status() !== 204) throw new Error(`the DELETE answered ${(await sent).status()}, not 204`);
        if (deletes.length !== 1) throw new Error(`expected one DELETE, saw ${JSON.stringify(deletes)}`);
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
