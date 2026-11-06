import { eachView, visible, type Scenario } from '../scenario.ts';

const scenario: Scenario = {
  name: 'contracts',
  stack: {},
  async run(ctx) {
    await eachView(ctx, 'contracts empty state', async (page, label) => {
      await page.goto(`${ctx.url}/#/contracts`);
      await visible(page.getByText('A contract is the agreed shape of an interface'));
      await ctx.shot(page, `contracts-${label}`);
    });
    await eachView(ctx, 'new contract dialog', async (page, label) => {
      await page.goto(`${ctx.url}/#/contracts`);
      await page.getByRole('button', { name: 'New contract' }).click();
      await visible(page.getByRole('dialog', { name: 'New contract' }));
      await ctx.shot(page, `contracts-new-${label}`);
    });
  },
};
export default scenario;
