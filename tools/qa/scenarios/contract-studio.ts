import { eachView, visible, type Scenario } from '../scenario.ts';

const SCHEDULE = '#/s/contracts%3Aqa-schedule';
const SDK = '#/s/contracts%3Aqa-sdk';

// Two seeded contracts, each with version 1 approved and version 2 passed and waiting for approval.
const scenario: Scenario = {
  name: 'contract-studio',
  stack: { seed: 'contracts' },
  async run(ctx) {
    await eachView(ctx, 'contract beside the chat, changes since v1 highlighted, approvable', async (page, label) => {
      await page.goto(`${ctx.url}/${SCHEDULE}`);
      const pane = page.getByRole('region', { name: 'Contract' });
      await visible(pane.getByRole('region', { name: 'Approval' }).getByText('Version 2 passed its checks.'));
      if (!(await pane.getByRole('button', { name: 'Approve' }).isEnabled())) throw new Error('Approve is disabled');
      await pane.getByRole('combobox', { name: 'Compare from' }).selectOption({ label: 'Version 1' });
      await visible(pane.getByRole('heading', { name: /since version 1$/ }));
      await visible(pane.getByText('Changed').first());
      await ctx.shot(page, `studio-rest-${label}`);
      if (label.startsWith('400')) {
        await page.getByRole('tab', { name: /^Chat/ }).click();
        await visible(page.getByRole('textbox', { name: 'Message' }));
        await ctx.shot(page, `studio-rest-chat-${label}`);
      } else await visible(page.getByRole('textbox', { name: 'Message' }));
    });
    await eachView(ctx, 'TypeScript contract: diagram centres on a type and re-centres on a click', async (page, label) => {
      await page.goto(`${ctx.url}/${SDK}`);
      const diagram = page.getByRole('region', { name: 'Class diagram' });
      await visible(diagram.getByRole('heading', { name: /around ExtensionManifest/ }));
      await ctx.shot(page, `studio-ts-${label}`);
      await diagram.getByRole('button', { name: 'Centre the diagram on App', exact: true }).click();
      await visible(diagram.getByRole('heading', { name: /around App$/ }));
      await diagram.screenshot({ path: await ctx.shot(page, `studio-ts-app-${label}`).then((p) => p.replace(/\.png$/, '-diagram.png')) });
    });
    await ctx.step('approving the waiting version records it', async () => {
      const context = await ctx.browser.newContext({ viewport: { width: 1280, height: 800 } });
      try {
        const page = await context.newPage();
        await page.goto(`${ctx.url}/${SCHEDULE}`);
        const bar = page.getByRole('region', { name: 'Contract' }).getByRole('region', { name: 'Approval' });
        await bar.getByRole('button', { name: 'Approve' }).click();
        await visible(bar.getByText('Version 2 is approved.'));
        await ctx.shot(page, 'studio-approved-1280');
      } finally {
        await context.close();
      }
    });
  },
};
export default scenario;
