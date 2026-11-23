import { eachView, visible, type Scenario } from '../scenario.ts';

// The shipped bundles plus one QA-only agent pinning its own model: the sleeper-* agents group under one heading, each row's
// status is a mark, never words, and only the pinned row names a model, which fits a 400 px row.
const pinned = { name: 'qa-pinned', version: 1, model: 'local/qa-pinned', prompt: 'prompt.md', extensions: {} };
const scenario: Scenario = {
  name: 'agents',
  stack: { agents: { 'qa-pinned': { definition: pinned, prompt: 'You are a QA agent.\n' } } },
  async run(ctx) {
    await eachView(ctx, 'agents list: grouped by prefix, one version and mark per row', async (page, label) => {
      await page.goto(`${ctx.url}/#/agents`);
      const main = page.getByRole('main');
      const sleeper = main.getByRole('region', { name: 'sleeper agents' });
      await visible(sleeper.getByRole('link', { name: /digest/ }).getByRole('img', { name: /^(Draft|Promoted: runs unattended)$/ }));
      if (await main.getByRole('link').getByText(/^(Draft|Promoted)$/).count()) throw new Error('a row prints its status');
      // The shipped bundles pin no model, so the lead names the default once and only the pinned row names its model.
      await visible(main.getByText(/most on the default model/));
      await visible(main.getByRole('link', { name: /qa-pinned/ }).getByText('local/qa-pinned'));
      if (await main.getByRole('link', { name: /reviewer/ }).getByText(/model|local\//).count()) throw new Error('a default-model row names a model');
      if (await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)) throw new Error('the page scrolls sideways');
      await ctx.shot(page, `agents-${label}`);
    });
  },
};
export default scenario;
