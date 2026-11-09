import type { Page } from 'playwright';
import { eachView, visible, type Ctx, type Scenario } from '../scenario.ts';

// The two seeded contracts give Sessions rows and Artifacts their specs and approvals.
// `gone` is text the filtered list no longer shows, so the shot waits for the server's answer.
async function filter(ctx: Ctx, page: Page, name: string, field: string, option: string, label: string, gone?: string) {
  const main = page.getByRole('main');
  await visible(main.getByRole('searchbox', { name: `Search ${name}` }));
  await ctx.shot(page, `${name}-closed-${label}`);
  await main.getByRole('button', { name: 'Filter' }).click();
  const panel = page.getByRole('dialog', { name: 'Filters' });
  await panel.getByRole('combobox', { name: field }).selectOption({ label: option });
  await visible(main.getByRole('button', { name: 'Filter, 1 set' }));
  if (gone) await main.getByText(gone).first().waitFor({ state: 'hidden', timeout: 10_000 });
  await ctx.shot(page, `${name}-open-${label}`);
  await page.keyboard.press('Escape');
  await visible(main.getByRole('button', { name: `Remove ${field}: ${option}` }));
  await ctx.shot(page, `${name}-chip-${label}`);
}

const scenario: Scenario = {
  name: 'search-filter',
  stack: { seed: 'contracts', apps: ['contracts'] },
  async run(ctx) {
    await eachView(ctx, 'sessions: search, funnel panel, agent chip', async (page, label) => {
      await page.goto(`${ctx.url}/#/sessions`);
      await visible(page.getByRole('main').getByRole('link').first());
      await filter(ctx, page, 'sessions', 'Agent', 'contracts', label);
    });
    await eachView(ctx, 'artifacts: search, funnel panel, kind chip', async (page, label) => {
      await page.goto(`${ctx.url}/#/artifacts`);
      await visible(page.getByRole('main').getByRole('button', { name: /^Open / }).first());
      await filter(ctx, page, 'artifacts', 'Kind', 'Spec', label, 'Approved version 1');
    });
  },
};
export default scenario;
