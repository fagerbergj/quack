import type { Page } from 'playwright';
import { eachView, visible, WITH_WIDE, type Scenario } from '../scenario.ts';
import { LEAGUE } from '../seed-artifacts.ts';

const s = (key: string) => `#/s/${encodeURIComponent(key)}`;
// Each seeded session, the pane (and phone tab) its newest version opens in, and something drawn inside it.
const PANES = [
  { name: 'review', key: 'github:fagerbergj/quack#2243', pane: 'Review', shows: 'ready to merge' },
  { name: 'research', key: 'research:qa-langfuse', pane: 'Report', shows: 'Short answer' },
  { name: 'contract', key: 'contracts:qa-schedule', pane: 'Contract', shows: 'Version 2 passed its checks.' },
  { name: 'sleeper', key: `sleeper:${LEAGUE}:8:lineup`, pane: 'Lineup picks', shows: 'Joe Burrow' },
];

// No github or sleeper service runs in the rig, so their page reads are answered here.
async function fakeServices(page: Page): Promise<void> {
  await page.route('**/apps/github/pulls**', (r) => r.fulfill({ json: { pulls: { 'fagerbergj/quack#2243': { state: 'open', at: '2026-11-08T12:00:00Z' } } } }));
  await page.route('**/apps/sleeper**', (r) => r.fulfill({ json: {} }));
}

const phone = (label: string) => label.startsWith('400');

// The PR chip on the header's 18px facts line must still take a tap 12px above and below its centre: a 24px target.
async function chipTarget(page: Page): Promise<void> {
  const chip = page.locator('main [title^="Pull request"]').first();
  await visible(chip);
  const box = (await chip.boundingBox())!;
  const at = { x: box.x + box.width / 2, y: box.y + box.height / 2 };
  const hits = await page.evaluate(({ x, y }) => [y - 12, y + 12].map((py) => Boolean(document.elementFromPoint(x, py)?.closest('[title^="Pull request"]'))), at);
  if (hits.includes(false)) throw new Error(`the PR chip's hit area is under 24px (box ${box.height}px, hits ${hits.join(',')})`);
}

const scenario: Scenario = {
  name: 'artifact-panes',
  stack: { seed: 'artifacts', apps: ['contracts'] },
  async run(ctx) {
    for (const p of PANES) {
      await eachView(ctx, `${p.name}: the newest version beside the chat, its own tab on a phone`, async (page, label) => {
        await fakeServices(page);
        await page.goto(`${ctx.url}/${s(p.key)}`);
        const pane = page.getByRole('region', { name: p.pane, exact: true });
        await visible(pane.getByText(p.shows).first());
        if (phone(label) && (await page.getByRole('tab', { name: new RegExp(`^${p.pane}`) }).getAttribute('aria-selected')) !== 'true') throw new Error(`the ${p.pane} tab is not open`);
        if (!phone(label)) await visible(page.getByRole('separator', { name: 'Resize the chat' }));
        if (p.name === 'review') await chipTarget(page);
        await ctx.shot(page, `${p.name}-${label}`);
      }, WITH_WIDE);
    }
    await eachView(ctx, 'a pending question opens a report session on Chat, counted on its tab', async (page, label) => {
      await page.goto(`${ctx.url}/${s('research:qa-ask')}`);
      await visible(page.getByRole('main').getByRole('region', { name: 'Question' }).getByText('Should self-hosting cost count').first());
      if (phone(label)) {
        if ((await page.getByRole('tab', { name: 'Chat 1' }).getAttribute('aria-selected')) !== 'true') throw new Error('the Chat tab is not open');
        await ctx.shot(page, `question-chat-${label}`);
        await page.getByRole('tab', { name: 'Report' }).click();
        await visible(page.getByRole('region', { name: 'Report' }).getByText('Short answer'));
      }
      await ctx.shot(page, `question-${label}`);
    });
    await eachView(ctx, 'a contract question opens on the Contract tab, where it is answered', async (page, label) => {
      await page.goto(`${ctx.url}/${s('contracts:qa-draft')}`);
      await visible(page.getByRole('region', { name: 'Contract' }).getByRole('region', { name: 'Question' }).first());
      if (phone(label) && (await page.getByRole('tab', { name: 'Contract 1' }).getAttribute('aria-selected')) !== 'true') throw new Error('the Contract tab is not open');
      await ctx.shot(page, `contract-question-${label}`);
    });
    await ctx.step('the divider resizes the chat from the keyboard and the width survives a reload', async () => {
      const context = await ctx.browser.newContext({ viewport: { width: 1280, height: 800 } });
      try {
        const page = await context.newPage();
        await page.goto(`${ctx.url}/${s('research:qa-langfuse')}`);
        const bar = page.getByRole('separator', { name: 'Resize the chat' });
        await bar.focus();
        for (let i = 0; i < 4; i++) await page.keyboard.press('ArrowRight');
        const width = await bar.getAttribute('aria-valuenow');
        if (width !== '536') throw new Error(`width after four steps: ${width}`);
        await page.reload();
        await visible(bar);
        if ((await bar.getAttribute('aria-valuenow')) !== width) throw new Error('the width was not remembered');
        await ctx.shot(page, 'divider-wider-1280');
      } finally {
        await context.close();
      }
    });
    await eachView(ctx, 'the Artifacts list opens an artifact on its own page; Back returns to the list', async (page, label) => {
      await fakeServices(page);
      await page.goto(`${ctx.url}/#/artifacts`);
      await page.getByRole('link', { name: /^Open #2243/ }).click();
      await page.waitForURL(/#\/s\/github%3Afagerbergj%2Fquack%232243\/a\/github\.review$/);
      await visible(page.getByRole('main').getByText('ready to merge').first());
      await visible(page.getByRole('link', { name: 'Open its session' }));
      await ctx.shot(page, `artifact-page-${label}`);
      await page.goBack();
      await visible(page.getByRole('link', { name: /^Open #2243/ }));
      await page.goForward();
      await visible(page.getByRole('main').getByText('ready to merge').first());
    }, WITH_WIDE);
  },
};
export default scenario;
