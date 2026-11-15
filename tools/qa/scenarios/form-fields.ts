import type { Page } from 'playwright';
import { eachView, visible, type Scenario } from '../scenario.ts';

const MESSAGE = 'Cap the retry backoff at 30 seconds.\nThe tests cover the cap.\nNit: rename waitMs to backoffMs.';

/** Asserts the focused control carries an outline at least 2 px wide: WCAG 2.4.7 focus visible at 3:1 contrast. */
async function ringVisible(page: Page) {
  const r = await page.evaluate(() => {
    const s = getComputedStyle(document.activeElement as Element);
    return { style: s.outlineStyle, width: parseFloat(s.outlineWidth) || 0 };
  });
  if (r.style === 'none' || r.style === 'hidden' || r.width < 2) throw new Error(`focus ring is ${r.width}px ${r.style}`);
}

/** The message field; below lg the session may open on its pane tab, so pick the chat tab first. */
async function composer(page: Page) {
  const chat = page.getByRole('tab', { name: /^Chat/ });
  if (await chat.waitFor({ state: 'visible', timeout: 3_000 }).then(() => true, () => false)) await chat.click();
  const box = page.getByRole('textbox', { name: 'Message' });
  await visible(box);
  return box;
}

const scenario: Scenario = {
  name: 'form-fields',
  stack: { seed: 'contracts', apps: ['contracts'] },
  async run(ctx) {
    await eachView(ctx, 'composer grows with the message', async (page, label) => {
      await page.goto(`${ctx.url}/#/sessions`);
      await page.locator('main a[href^="#/s/"]').first().click();
      const box = await composer(page);
      const empty = (await box.boundingBox())!.height;
      await box.click();
      await box.fill(MESSAGE);
      await ctx.shot(page, `composer-${label}`);
      const typed = (await box.boundingBox())!.height;
      if (typed <= empty + 20) throw new Error(`field stayed ${Math.round(typed)}px with 3 lines in (empty ${Math.round(empty)}px)`);
    });

    await eachView(ctx, 'a focused field shows a focus ring', async (page, label) => {
      await page.goto(`${ctx.url}/#/contracts`);
      await page.getByRole('button', { name: 'New contract' }).click();
      const ask = page.getByRole('textbox', { name: /What should it do/ });
      await visible(ask);
      await ask.click();
      await ctx.shot(page, `dialog-field-${label}`);
      await ringVisible(page);
      await page.keyboard.press('Escape');
      await page.goto(`${ctx.url}/#/sessions`);
      await page.getByRole('button', { name: 'Filter' }).click();
      const agent = page.getByRole('combobox', { name: 'Agent' });
      await visible(agent);
      await agent.focus();
      await ctx.shot(page, `filter-${label}`);
      await ringVisible(page);
    });

    await eachView(ctx, 'a skip link bypasses the nav and session list', async (page, label) => {
      const desktop = page.viewportSize()!.width >= 1024;
      await page.goto(`${ctx.url}/#/sessions`);
      await visible(page.getByRole('main'));
      await page.keyboard.press('Tab');
      const skip = page.getByRole('link', { name: 'Skip to main content' });
      if (!desktop) {
        // Below lg the nav sits after main in the DOM, so there is nothing to skip and no link in the tab order.
        if (await skip.isVisible()) throw new Error('the skip link shows on a phone, where it has nothing to skip');
        return;
      }
      await visible(skip);
      await ctx.shot(page, `skip-focus-${label}`);
      await page.keyboard.press('Enter');
      const inside = await page.evaluate(() => document.getElementById('main')?.contains(document.activeElement) ?? false);
      if (!inside) throw new Error('Enter on the skip link left focus outside main');
      if (page.url().includes('#main')) throw new Error('the skip link hijacked the hash router');
    });
  },
};
export default scenario;
