import type { FakeScript } from '../fake-model.ts';
import type { Locator, Page } from 'playwright';
import { eachView, paneOf, visible, type Ctx, type Scenario } from '../scenario.ts';

const SCHEDULE = '#/s/contracts%3Aqa-schedule';
const SDK = '#/s/contracts%3Aqa-sdk';
const DRAFT = '#/s/contracts%3Aqa-draft';

const text = (m?: { content?: unknown }) => (typeof m?.content === 'string' ? m.content : JSON.stringify(m?.content ?? ''));
const no = { choice: 'no', confidence: 1, reason: 'No field, status code or schema rule in the document carries it.' };
// The model checks an approve runs fail every guarantee ("Decision:" prompts); any other turn just answers.
const script: FakeScript = ({ messages = [] }) => {
  const asked = text(messages.at(-1));
  if (asked.startsWith('Decision:')) return { content: JSON.stringify(no) };
  return { content: 'Noted.' };
};

// Presses Approve on the schedule contract's bar at 1280 px, then `button` in the sheet; `after` checks what the page shows.
async function approveSchedule(ctx: Ctx, button: string, after: (page: Page, bar: Locator) => Promise<void>) {
  const context = await ctx.browser.newContext({ viewport: { width: 1280, height: 800 } });
  try {
    const page = await context.newPage();
    await page.goto(`${ctx.url}/${SCHEDULE}`);
    const bar = paneOf(page, 'Contract').getByRole('region', { name: 'Approval' });
    await bar.getByRole('button', { name: 'Approve' }).click();
    await page.getByRole('dialog', { name: 'Approve v2?' }).getByRole('button', { name: button }).click();
    await after(page, bar);
  } finally {
    await context.close();
  }
}

// Two seeded contracts, each with version 1 approved and version 2 passed and waiting for approval, and one with no version and a question waiting.
const scenario: Scenario = {
  name: 'contract-studio',
  stack: { seed: 'contracts', apps: ['contracts'], script },
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
      // A version is its number and a mark; its standing is the spoken name, and the approved one it is compared with is the "since" chip.
      await visible(pane.getByRole('button', { name: 'Compare with: v1, approved' }).getByText('since v1', { exact: true }));
      await pane.getByRole('button', { name: 'Version: v2, needs you' }).click();
      const items = pane.getByRole('menu', { name: 'Version' }).getByRole('menuitem');
      const labels = await items.evaluateAll((els) => els.map((e) => e.getAttribute('aria-label') ?? e.textContent));
      if (labels.join('|') !== 'v2, needs you|v1, approved|Compare with…') throw new Error(`version labels: ${labels.join(', ')}`);
      if ((await items.allTextContents()).join('|') !== 'v2|v1|Compare with…') throw new Error('a menu row shows more than its number');
      await ctx.shot(page, `studio-rest-versions-${label}`);
      await items.getByText('Compare with…').click();
      await visible(pane.getByRole('menu', { name: 'Compare v2 with' }).getByRole('menuitem', { name: 'v1, approved' }));
      await ctx.shot(page, `studio-rest-compare-with-${label}`);
      await page.keyboard.press('Escape');
      // The seeded break names `runs[].at`, so the changed field is marked as breaking a caller.
      const at = pane.locator('li[data-ptr$="/properties/at"]');
      await visible(at.getByRole('img', { name: 'Breaks a caller' }));
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
      await visible(pane.locator('#decl-App').getByRole('img', { name: 'New' }).first());
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
    await ctx.step('a message sent while a version waits withdraws it with no reject from the page and no error banner', async () => {
      const context = await ctx.browser.newContext({ viewport: { width: 1280, height: 800 } });
      try {
        const page = await context.newPage();
        const decisions: string[] = [];
        page.on('request', (r) => void (r.method() === 'PATCH' && decisions.push(r.url())));
        await page.goto(`${ctx.url}/${SDK}`);
        const bar = paneOf(page, 'Contract').getByRole('region', { name: 'Approval' });
        await visible(bar.getByRole('button', { name: 'Approve', exact: true }));
        await page.getByRole('textbox', { name: 'Message' }).fill('Why is App a class and not an interface?');
        await page.getByRole('button', { name: 'Send' }).click();
        await bar.getByRole('button', { name: 'Approve', exact: true }).waitFor({ state: 'detached', timeout: 15_000 });
        // The catalog hears the cancel from the session's lifecycle stream, a moment after the page does.
        let v2: Array<{ state: string }> = [];
        for (let i = 0; i < 30 && (!v2.length || v2.some((a) => a.state === 'pending')); i++, await new Promise((r) => setTimeout(r, 500))) {
          const { asks = [] } = (await (await fetch(`${ctx.url}/sessions/${encodeURIComponent('contracts:qa-sdk')}/asks`)).json()) as { asks?: Array<{ state: string; toolCall?: { args?: { version?: number } } }> };
          v2 = asks.filter((a) => a.toolCall?.args?.version === 2);
        }
        if (!v2.length || v2.some((a) => a.state !== 'cancelled')) throw new Error(`v2's asks read ${JSON.stringify(v2)}, not cancelled`);
        if (decisions.length) throw new Error(`the page sent decisions: ${decisions.join(', ')}`);
        if (await page.getByRole('alert').filter({ hasText: /withdrawn/ }).count()) throw new Error('an error banner names the withdrawal');
        await ctx.shot(page, 'studio-message-withdraws-1280');
      } finally {
        await context.close();
      }
    });
    await ctx.step('approving the waiting version runs the model checks, and their findings hold it open', () => approveSchedule(ctx, 'Approve v2', async (page, bar) => {
      await bar.getByRole('button', { name: 'Model checks failed' }).waitFor({ timeout: 60_000 });
      if (await page.getByRole('button', { name: 'Version: v2, approved' }).count()) throw new Error('v2 was approved over failing checks');
    }));
    await eachView(ctx, 'the approval with findings: failing items first, Approve anyway and Send to the agent', async (page, label) => {
      await page.goto(`${ctx.url}/${SCHEDULE}`);
      const bar = paneOf(page, 'Contract').getByRole('region', { name: 'Approval' });
      await visible(bar.getByRole('button', { name: 'Model checks failed' }));
      await ctx.shot(page, `studio-findings-bar-${label}`);
      await bar.getByRole('button', { name: 'Model checks failed' }).click();
      const sheet = page.getByRole('dialog', { name: 'Approve v2?' });
      await visible(sheet.getByRole('heading', { name: 'Model checks' }));
      await visible(sheet.getByText('The revision must fix'));
      if (await sheet.getByRole('button', { name: 'Approve v2' }).count()) throw new Error('a plain approve is offered over the findings');
      await visible(sheet.getByRole('button', { name: 'Approve anyway' }));
      await visible(sheet.getByRole('button', { name: 'Send to the agent' }));
      await ctx.shot(page, `studio-findings-sheet-${label}`);
    }, [{ width: 1280, height: 800 }, { width: 390, height: 800 }]);
    await ctx.step('Approve anyway records the waiting version over the findings', () => approveSchedule(ctx, 'Approve anyway', async (page, bar) => {
      await visible(page.getByRole('button', { name: 'Version: v2, approved' }));
      await visible(bar.getByText('v2 is approved'));
      await ctx.shot(page, 'studio-approved-1280');
    }));
  },
};
export default scenario;
