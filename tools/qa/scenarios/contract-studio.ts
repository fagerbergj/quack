import { eachView, paneOf, visible, type Scenario } from '../scenario.ts';

const SCHEDULE = '#/s/contracts%3Aqa-schedule';
const SDK = '#/s/contracts%3Aqa-sdk';
const DRAFT = '#/s/contracts%3Aqa-draft';

// Two seeded contracts, each with version 1 approved and version 2 passed and waiting for approval, and one with no version and a question waiting.
const scenario: Scenario = {
  name: 'contract-studio',
  stack: { seed: 'contracts', apps: ['contracts'] },
  async run(ctx) {
    await eachView(ctx, 'contract beside the chat, changes since v1 highlighted, approvable', async (page, label) => {
      await page.goto(`${ctx.url}/${SCHEDULE}`);
      const pane = paneOf(page, 'Contract');
      await visible(pane.getByRole('region', { name: 'Approval' }).getByText('Version 2 passed its checks.'));
      if (!(await pane.getByRole('button', { name: 'Approve', exact: true }).isEnabled())) throw new Error('Approve is disabled');
      // A viewer who never looked is shown what changed since the approved version, each version labelled by its status.
      await visible(pane.getByRole('heading', { name: /^\d+ changes? since the approved version$/ }));
      await visible(pane.getByText('GET /apps/sleeper/schedule › 200 › runs › at › type'));
      await visible(pane.locator('del').first());
      await ctx.shot(page, `studio-rest-${label}`);
      await pane.getByRole('button', { name: /^Compare from: / }).click();
      const labels = await pane.getByRole('menu', { name: 'Compare from' }).getByRole('menuitem').allTextContents();
      if (labels.join('|') !== 'None: the whole contract|Version 2awaiting, awaiting approval|Version 1approved, approved') throw new Error(`version labels: ${labels.join(', ')}`);
      await ctx.shot(page, `studio-rest-versions-${label}`);
      await page.keyboard.press('Escape');
      await pane.getByRole('listitem').filter({ has: page.getByText('Changed', { exact: true }) }).first().scrollIntoViewIfNeeded();
      await ctx.shot(page, `studio-rest-changed-${label}`);
      if (label.startsWith('400')) {
        await page.getByRole('tab', { name: /^Chat/ }).click();
        await visible(page.getByRole('textbox', { name: 'Message' }));
        await ctx.shot(page, `studio-rest-chat-${label}`);
      } else await visible(page.getByRole('textbox', { name: 'Message' }));
    });
    await eachView(ctx, 'before the first version the Contract tab answers the waiting question, with the status, brief and decisions', async (page, label) => {
      await page.goto(`${ctx.url}/${DRAFT}`);
      const pane = paneOf(page, 'Contract');
      await visible(pane.getByRole('region', { name: 'Question' }).getByText('How do callers authenticate?'));
      await visible(pane.getByRole('region', { name: 'Progress' }).getByText('Waiting on you'));
      await visible(pane.getByText('Cursor or offset paging?'));
      if (label.startsWith('400') && (await page.getByRole('tab', { name: 'Contract 1' }).getAttribute('aria-selected')) !== 'true') throw new Error('the Contract tab is not open');
      await pane.getByText('Brief').click();
      await ctx.shot(page, `studio-draft-${label}`);
    });
    await eachView(ctx, 'the Contracts page shows each contract\'s kind, version and approval status', async (page, label) => {
      await page.goto(`${ctx.url}/#/contracts`);
      const row = page.getByRole('listitem').filter({ hasText: 'Sleeper schedule endpoint' });
      await visible(row.getByText('REST · v2'));
      await visible(row.getByText('Awaiting your approval'));
      await visible(page.getByRole('listitem').filter({ hasText: 'TypeScript · v2' }).first());
      await ctx.shot(page, `contracts-page-${label}`);
    });
    await eachView(ctx, 'TypeScript contract: diagram centres on a type and re-centres on a click', async (page, label) => {
      await page.goto(`${ctx.url}/${SDK}`);
      const diagram = page.getByRole('region', { name: 'Class diagram' });
      await visible(diagram.getByRole('heading', { name: /around ExtensionManifest/ }));
      const pane = paneOf(page, 'Contract');
      await visible(pane.getByRole('heading', { name: /^\d+ changes? since the approved version$/ }));
      await visible(pane.locator('#decl-App').getByText('New').first());
      await pane.getByRole('region', { name: 'What changed' }).scrollIntoViewIfNeeded();
      await ctx.shot(page, `studio-ts-changes-${label}`);
      await ctx.shot(page, `studio-ts-${label}`);
      await pane.locator('#decl-App').scrollIntoViewIfNeeded();
      await ctx.shot(page, `studio-ts-changed-${label}`);
      await diagram.getByRole('button', { name: 'Centre the diagram on App', exact: true }).click();
      await visible(diagram.getByRole('heading', { name: /around App$/ }));
      await diagram.screenshot({ path: await ctx.shot(page, `studio-ts-app-${label}`).then((p) => p.replace(/\.png$/, '-diagram.png')) });
    });
    await ctx.step('approving the waiting version records it', async () => {
      const context = await ctx.browser.newContext({ viewport: { width: 1280, height: 800 } });
      try {
        const page = await context.newPage();
        await page.goto(`${ctx.url}/${SCHEDULE}`);
        const bar = paneOf(page, 'Contract').getByRole('region', { name: 'Approval' });
        await bar.getByRole('button', { name: 'Approve' }).click();
        await visible(page.getByRole('button', { name: 'Compare to: Version 2, approved' }));
        await bar.waitFor({ state: 'detached' });
        await ctx.shot(page, 'studio-approved-1280');
      } finally {
        await context.close();
      }
    });
  },
};
export default scenario;
