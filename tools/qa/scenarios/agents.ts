import { eachView, visible, type Scenario } from '../scenario.ts';

// The shipped agent bundles: the sleeper-* agents group under one heading, and each row's status is a mark, never words.
const scenario: Scenario = {
  name: 'agents',
  stack: {},
  async run(ctx) {
    await eachView(ctx, 'agents list: grouped by prefix, one version and mark per row', async (page, label) => {
      await page.goto(`${ctx.url}/#/agents`);
      const main = page.getByRole('main');
      const sleeper = main.getByRole('region', { name: 'sleeper agents' });
      await visible(sleeper.getByRole('link', { name: /digest/ }).getByRole('img', { name: /^(Draft|Promoted: runs unattended)$/ }));
      if (await main.getByRole('link').getByText(/^(Draft|Promoted)$/).count()) throw new Error('a row prints its status');
      // The shipped bundles pin no model, so the lead says so once and no row names one.
      await visible(main.getByText(/all on the default model/));
      if (await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)) throw new Error('the page scrolls sideways');
      await ctx.shot(page, `agents-${label}`);
    });
  },
};
export default scenario;
