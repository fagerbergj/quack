import assert from 'node:assert/strict';
import { eachView, visible, type Scenario } from '../scenario.ts';

// The demo session's composer picks its model from the deployment's list, and Settings sets the server default.
const scenario: Scenario = {
  name: 'model-picker',
  stack: { seed: 'demo' },
  async run(ctx) {
    await eachView(ctx, 'composer model picker', async (page, label) => {
      await page.goto(`${ctx.url}/#/s/qa-review`);
      const picker = page.getByRole('combobox', { name: 'Model:' });
      await visible(picker);
      await picker.selectOption('');
      await ctx.shot(page, `composer-default-${label}`);
      await picker.selectOption('local/qwen3.8-27b');
      await page.waitForFunction(async () => (await (await fetch('sessions/qa-review')).json()).model === 'local/qwen3.8-27b');
      await ctx.shot(page, `composer-picked-${label}`);
      await picker.selectOption('');
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'no sideways scroll');
    });
    await eachView(ctx, 'settings default model', async (page, label) => {
      await page.goto(`${ctx.url}/#/settings`);
      const select = page.getByRole('combobox', { name: /Default for new runs/ });
      await visible(select);
      await ctx.shot(page, `settings-${label}`);
    });
  },
};
export default scenario;
