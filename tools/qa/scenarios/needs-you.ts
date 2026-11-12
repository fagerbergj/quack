import { devices } from 'playwright';
import { visible, type Scenario } from '../scenario.ts';

const KEY = 'research:qa-ask';
const NOTE = 'Only past the free tier.';

// The first answer stands, so the seeded question is answered once, on a phone.
const scenario: Scenario = {
  name: 'needs-you',
  stack: { seed: 'artifacts' },
  async run(ctx) {
    await ctx.step('Home answers a waiting question with an option and a note @pixel7-dark', async () => {
      const context = await ctx.browser.newContext({ ...devices['Pixel 7'], colorScheme: 'dark' });
      try {
        const page = await context.newPage();
        const errors: string[] = [];
        page.on('pageerror', (e) => errors.push(e.message));
        page.on('console', (m) => void (m.type() === 'error' && errors.push(m.text())));
        await page.goto(`${ctx.url}/#/`);
        const card = page.getByRole('main').getByRole('region', { name: 'Needs you' }).getByRole('region', { name: 'Question' }).filter({ hasText: 'Should self-hosting cost count' });
        await visible(card);
        await card.getByRole('button', { name: 'Add a note' }).click();
        await card.getByRole('textbox', { name: 'Note' }).fill(NOTE);
        await ctx.shot(page, 'needs-you-note-pixel7-dark');
        await card.getByRole('button', { name: 'Yes', exact: true }).click();
        await card.waitFor({ state: 'detached', timeout: 10_000 });
        await ctx.shot(page, 'needs-you-answered-pixel7-dark');
        const ask = (await (await fetch(`${ctx.url}/sessions/${encodeURIComponent(KEY)}/asks/qa%3Ascope`)).json()) as { state: string; resolution?: { choice: string; reason?: string } };
        if (ask.state !== 'resolved' || ask.resolution?.choice !== 'Yes' || ask.resolution.reason !== NOTE) throw new Error(`the ask reads ${JSON.stringify(ask)}`);
        if (await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)) throw new Error('the page scrolls sideways');
        if (errors.length) throw new Error(`page or console errors: ${errors.join(' | ')}`);
      } finally {
        await context.close();
      }
    });
  },
};
export default scenario;
