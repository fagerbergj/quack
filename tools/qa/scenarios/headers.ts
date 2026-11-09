import type { Page } from 'playwright';
import { eachView, visible, type Scenario } from '../scenario.ts';

const LEAGUE = '1356407594683482112';
// Two repos, so lists prefix the repo; the header still leads with the PR number and title.
const SESSIONS = [
  { name: 'review', key: 'github:fagerbergj/quack#2243', title: 'fix(judge): Every failing item is named in the verdict, with its rubric line', line1: '#2243 Every failing item is named' },
  { name: 'review-other', key: 'github:acme/tool#5', title: 'feat: add retries', line1: '#5 Add retries' },
  { name: 'research', key: 'research:qa-langfuse', title: 'What changed in the Langfuse v4 public ingestion API? Cite sources.', line1: 'What changed in the Langfuse v4' },
  { name: 'chat', key: 'home:qa-chat', title: 'Plan the week', line1: 'quack' },
  { name: 'sleeper', key: `sleeper:${LEAGUE}:8:lineup`, title: undefined, line1: 'Lineup · wk 8' },
];
const SHOWN = ['review', 'research', 'chat', 'sleeper'];

async function seed(url: string): Promise<void> {
  for (const s of SESSIONS) {
    const base = `${url}/sessions/${encodeURIComponent(s.key)}`;
    const res = await fetch(`${base}/invocations?agent=quack`, { method: 'POST', body: JSON.stringify({ input: 'Say hello', wait: true }) });
    if (!res.ok) throw new Error(`seed ${s.key}: ${res.status} ${await res.text()}`);
    if (!s.title) continue;
    const named = await fetch(base, { method: 'PATCH', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ title: s.title }) });
    if (!named.ok) throw new Error(`rename ${s.key}: ${named.status} ${await named.text()}`);
  }
}

// No github or sleeper service runs in the rig, so their page reads are answered here.
async function fakeServices(page: Page): Promise<void> {
  await page.route('**/apps/github/pulls**', (r) => r.fulfill({ json: { pulls: { 'fagerbergj/quack#2243': { state: 'open', at: '2026-11-08T12:00:00Z' } } } }));
  await page.route('**/apps/sleeper/schedule', (r) => r.fulfill({ json: { runs: [], week: 9 } }));
  await page.route('**/apps/sleeper', (r) => r.fulfill({ json: { league: LEAGUE } }));
  await page.route('**/apps/sleeper/waivers', (r) => r.fulfill({ json: { sessionKey: `sleeper:${LEAGUE}:9:waivers` } }));
}

const scenario: Scenario = {
  name: 'headers',
  stack: { seed: 'contracts', model: 'fake' },
  async run(ctx) {
    await ctx.step('seed sessions', () => seed(ctx.url));
    for (const s of SESSIONS.filter((x) => SHOWN.includes(x.name))) {
      await eachView(ctx, `${s.name} header`, async (page, label) => {
        await fakeServices(page);
        await page.goto(`${ctx.url}/#/s/${encodeURIComponent(s.key)}`);
        await visible(page.getByRole('heading', { level: 1 }).filter({ hasText: s.line1 }));
        await ctx.shot(page, `${s.name}-${label}`);
      });
    }
    await eachView(ctx, 'contract header', async (page, label) => {
      await page.goto(`${ctx.url}/#/s/${encodeURIComponent('contracts:qa-schedule')}`);
      // The pane's own header carries the file download; wait for the spec, not just the bar.
      await visible(page.getByRole('button', { name: /OpenAPI file/ }).first());
      await ctx.shot(page, `contract-${label}`);
    });
    await eachView(ctx, 'contracts page header', async (page, label) => {
      await page.goto(`${ctx.url}/#/contracts`);
      await visible(page.getByRole('button', { name: 'New contract' }));
      await ctx.shot(page, `contracts-page-${label}`);
    });
    await eachView(ctx, 'sleeper page header', async (page, label) => {
      await fakeServices(page);
      await page.goto(`${ctx.url}/#/sleeper`);
      await visible(page.getByRole('heading', { name: 'Week 9' }));
      await ctx.shot(page, `sleeper-page-${label}`);
      await page.getByRole('button', { name: 'Run a job' }).click();
      // The last row must clear the tab bar on a phone.
      await visible(page.getByRole('dialog', { name: 'Run a job' }).getByText('League history'));
      await ctx.shot(page, `sleeper-sheet-${label}`);
      await page.getByRole('button', { name: 'Run Waivers' }).click();
      await visible(page.getByRole('link', { name: /^Waivers Running · open/ }));
      await ctx.shot(page, `sleeper-sheet-started-${label}`);
    });
  },
};
export default scenario;
