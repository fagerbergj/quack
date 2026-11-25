import assert from 'node:assert/strict';
import type { Page } from 'playwright';
import { eachView, visible, type Scenario } from '../scenario.ts';

// The select shows its first option until the model it names has loaded, so wait for that one.
const shows = (page: Page, id: string, model: string) => page.waitForFunction(([i, m]) => (document.getElementById(i) as HTMLSelectElement | null)?.value === m, [id, model], { timeout: 10_000 });
const clearPick = () => fetch('sessions/qa-review', { method: 'PATCH', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ model: null }) }).then((r) => r.ok);

// The demo session's composer names the model it runs, offers the ones the fake provider lists, and Settings names the default.
const scenario: Scenario = {
  name: 'model-picker',
  stack: { seed: 'demo' },
  async run(ctx) {
    await eachView(ctx, 'composer model picker', async (page, label) => {
      await page.goto(`${ctx.url}/#/s/qa-review`);
      assert.equal(await page.evaluate(clearPick), true);
      await page.reload();
      const picker = page.getByRole('combobox', { name: 'Model:' });
      await visible(picker);
      await shows(page, 'model-pick', 'local/qwen3.8-flash-next');
      const options = await picker.locator('option').allTextContents();
      assert.deepEqual(options, ['qwen3.8-27b', 'qwen3.8-flash-next', 'qa-fake', 'deepseek-v4.1-flash'], 'the peer form, Clef and the hidden embedding model are not offered');
      await ctx.shot(page, `composer-resolved-${label}`);
      await picker.selectOption('local/peer/deepseek-v4.1-flash');
      await page.waitForFunction(async () => (await (await fetch('sessions/qa-review')).json()).model === 'local/peer/deepseek-v4.1-flash');
      await ctx.shot(page, `composer-picked-${label}`);
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'no sideways scroll');
    });
    await eachView(ctx, 'settings default model', async (page, label) => {
      await page.goto(`${ctx.url}/#/settings`);
      const select = page.getByRole('combobox', { name: /Default for new runs/ });
      await visible(select);
      await shows(page, 'default-model', 'local/qwen3.8-flash-next');
      await ctx.shot(page, `settings-${label}`);
    });
  },
};
export default scenario;
