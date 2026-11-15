import { eachView, visible, type Scenario } from '../scenario.ts';

const scenario: Scenario = {
  name: 'remarkable',
  stack: { apps: ['remarkable'] },
  async run(ctx) {
    await eachView(ctx, 'reMarkable page', async (page, label) => {
      await page.goto(`${ctx.url}/#/remarkable`);
      await visible(page.locator('main').getByText('Campaign Notes').first());
      await ctx.shot(page, `remarkable-${label}`);
    });
    await ctx.step('ingest and topics routes answer as the v5 contract says', async () => {
      const app = `${ctx.url}/apps/remarkable`;
      const post = (path: string, body: unknown) => fetch(`${app}${path}`, { method: 'POST', body: JSON.stringify(body) });
      const unknown = await post('/ingest', { ids: ['qa-notes', 'ghost'] });
      const problem = (await unknown.json()) as { ids?: string[] };
      if (unknown.status !== 404 || unknown.headers.get('content-type') !== 'application/problem+json' || problem.ids?.join() !== 'ghost') throw new Error(`unknown ids answered ${unknown.status} ${JSON.stringify(problem)}`);
      const closed = await post('/ingest', { ids: ['qa-notes'], extra: 1 });
      if (closed.status !== 400) throw new Error(`an unknown field answered ${closed.status}, not 400`);
      const topics = await fetch(`${app}/topics`);
      if (topics.status !== 401) throw new Error(`a credential-free GET /topics answered ${topics.status}, not the gateway's 401`);
    });
  },
};
export default scenario;
