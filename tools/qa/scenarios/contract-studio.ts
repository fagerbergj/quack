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
      await visible(pane.getByRole('region', { name: 'Approval' }).getByText('v2 waits on you'));
      if (!(await pane.getByRole('button', { name: 'Approve', exact: true }).isEnabled())) throw new Error('Approve is disabled');
      // The changes since the approved version lead the page, one line each; Next change goes to each in the spec.
      const changes = pane.getByRole('region', { name: 'Changes against v1' });
      await visible(changes.getByRole('button', { name: 'GET /apps/sleeper/schedule › 200 › runs › at: type and description changed' }));
      await visible(changes.getByRole('heading', { name: 'What could break' }));
      await ctx.shot(page, `studio-rest-${label}`);
      await pane.getByRole('button', { name: 'First change' }).click();
      await visible(pane.getByText(/^Change 1 of \d+ against v1$/));
      await ctx.shot(page, `studio-rest-first-change-${label}`);
      await pane.getByRole('region', { name: 'Approval' }).getByRole('button', { name: 'Approve', exact: true }).click();
      const sheet = page.getByRole('dialog', { name: 'Approve v2?' });
      await visible(sheet.getByText('REST contract, measured against v1.'));
      await ctx.shot(page, `studio-rest-approve-sheet-${label}`);
      await sheet.getByRole('button', { name: 'Not yet' }).click();
      await pane.getByRole('button', { name: /^Compare from version: / }).click();
      const labels = await pane.getByRole('menu', { name: 'Compare from version' }).getByRole('menuitem').allTextContents();
      if (labels.join('|') !== 'None: the whole contract|Version 2awaiting, awaiting approval|Version 1approved, approved') throw new Error(`version labels: ${labels.join(', ')}`);
      await ctx.shot(page, `studio-rest-versions-${label}`);
      await page.keyboard.press('Escape');
      // The seeded break names `runs[].at`, so the changed field is marked as breaking a caller.
      const at = pane.locator('li[data-ptr$="/properties/at"]');
      await visible(at.getByText('Breaks a caller', { exact: true }));
      await at.scrollIntoViewIfNeeded();
      await ctx.shot(page, `studio-rest-changed-${label}`);
      if (label.startsWith('400')) {
        await page.getByRole('tab', { name: /^Chat/ }).click();
        await visible(page.getByRole('textbox', { name: 'Message' }));
        await ctx.shot(page, `studio-rest-chat-${label}`);
      } else await visible(page.getByRole('textbox', { name: 'Message' }));
    });
    await eachView(ctx, "the guarantees sheet shows each fact's text with its id beside it, grouped by the ids' dotted prefix", async (page, label) => {
      await page.goto(`${ctx.url}/${SCHEDULE}`);
      await paneOf(page, 'Contract').getByRole('button', { name: /^Guarantees/ }).click();
      const sheet = page.getByRole('dialog', { name: 'Guarantees' });
      await visible(sheet.getByRole('listitem').filter({ hasText: 'Errors say why in problem+json' }).locator('code', { hasText: 'errors.problem-json' }));
      await visible(sheet.getByRole('heading', { name: 'errors', level: 3 }));
      await ctx.shot(page, `studio-rest-guarantees-${label}`);
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
    await eachView(ctx, 'the Contracts page shows each contract\'s kind, one version and a mark that names its status', async (page, label) => {
      await page.goto(`${ctx.url}/#/contracts`);
      const busy = page.getByRole('region', { name: 'In progress' });
      const row = busy.getByRole('link', { name: /Sleeper schedule endpoint/ });
      await visible(row.getByText('REST'));
      await visible(row.getByText('v2'));
      await visible(row.getByRole('img', { name: 'Awaiting your approval' }));
      await visible(busy.getByRole('link', { name: /^TS/ }).first());
      if (await busy.getByText(/Awaiting|Approved v/).count()) throw new Error('a row prints its status');
      await ctx.shot(page, `contracts-page-${label}`);
    });
    await eachView(ctx, 'TypeScript contract: diagram centres on a type and re-centres on a click', async (page, label) => {
      await page.goto(`${ctx.url}/${SDK}`);
      const diagram = page.getByRole('region', { name: 'Class diagram' });
      await visible(diagram.getByRole('heading', { name: /around ExtensionManifest/ }));
      const pane = paneOf(page, 'Contract');
      await visible(pane.locator('#decl-App').getByText('New').first());
      await pane.getByRole('region', { name: 'Changes against v1' }).scrollIntoViewIfNeeded();
      await ctx.shot(page, `studio-ts-changes-${label}`);
      await ctx.shot(page, `studio-ts-${label}`);
      await pane.locator('#decl-App').scrollIntoViewIfNeeded();
      await ctx.shot(page, `studio-ts-changed-${label}`);
      await diagram.getByRole('button', { name: /^Centre the diagram on App(,|$)/ }).click();
      await visible(diagram.getByRole('heading', { name: /around App$/ }));
      await diagram.screenshot({ path: await ctx.shot(page, `studio-ts-app-${label}`).then((p) => p.replace(/\.png$/, '-diagram.png')) });
    });
    await eachView(ctx, 'a long contract title is clamped on a phone and expands in place on a tap, and is plain text on a desktop', async (page, label) => {
      await page.goto(`${ctx.url}/${SDK}`);
      const title = paneOf(page, 'Contract').getByRole('heading', { level: 2, name: /^Extension SDK manifest: / });
      if (label.startsWith('1280')) {
        await visible(title);
        if (await title.getByRole('button', { name: /^Extension SDK manifest/ }).count()) throw new Error('the title is a control on a desktop');
        return;
      }
      const toggle = title.getByRole('button', { name: /^Extension SDK manifest/ });
      await visible(toggle);
      if ((await toggle.getAttribute('aria-expanded')) !== 'false') throw new Error('the title starts open');
      await ctx.shot(page, `studio-title-collapsed-${label}`);
      await toggle.click();
      if ((await toggle.getAttribute('aria-expanded')) !== 'true') throw new Error('a tap did not open the title');
      await ctx.shot(page, `studio-title-expanded-${label}`);
      await toggle.click();
      if ((await toggle.getAttribute('aria-expanded')) !== 'false') throw new Error('a second tap did not close the title');
    }, [{ width: 1280, height: 800 }, { width: 390, height: 800 }]);
    await ctx.step('approving the waiting version records it', async () => {
      const context = await ctx.browser.newContext({ viewport: { width: 1280, height: 800 } });
      try {
        const page = await context.newPage();
        await page.goto(`${ctx.url}/${SCHEDULE}`);
        const bar = paneOf(page, 'Contract').getByRole('region', { name: 'Approval' });
        await bar.getByRole('button', { name: 'Approve' }).click();
        await page.getByRole('dialog', { name: 'Approve v2?' }).getByRole('button', { name: 'Approve v2' }).click();
        await visible(page.getByRole('button', { name: 'Compare to version: Version 2, approved' }));
        await visible(bar.getByText('v2 is approved'));
        await ctx.shot(page, 'studio-approved-1280');
      } finally {
        await context.close();
      }
    });
  },
};
export default scenario;
