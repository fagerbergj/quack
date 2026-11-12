import type { ExtensionManifest } from '@quack/sdk';
import type { Workflow } from '@quack/workflows';
import { sleeperApp } from './app.ts';
import { settingsProblems } from './settings.ts';
import { runJob } from './action.ts';
import { withChat } from './chat.ts';
import { HISTORY_WORKFLOW, historyFromEnv } from './history/index.ts';
import { LINEUP_WORKFLOW, lineupFromEnv } from './lineup/index.ts';
import { DIGEST_WORKFLOW, digestFromEnv, RETRO_WORKFLOW, retroFromEnv } from './report/index.ts';
import { FINDER_WORKFLOW, finderFromEnv } from './trades/index.ts';
import { TRENDS_WORKFLOW, trendsFromEnv } from './trends/index.ts';
import { WAIVERS_WORKFLOW, waiversFromEnv } from './waivers/index.ts';
import { cardAdapters } from './adapters.ts';

const env = ['QUACK_EXA_API_KEY'];
/** A card job built from the environment, judged with the decision model its agent's workflows config names. */
const job = (fromEnv: (env: NodeJS.ProcessEnv, config: never) => Workflow) => ({ build: (config: unknown) => withChat(fromEnv(process.env, config as never)), env });

/** Sleeper contributes workflows and an eval adapter per judged card; an agent binds one through the workflows extension. */
export const manifest: ExtensionManifest = {
  name: 'sleeper',
  requires: ['workflows'],
  optional: ['judge'],
  settings: settingsProblems,
  app: sleeperApp,
  peers: ['schedules'],
  pages: [{ method: 'GET', path: '/' }, { method: 'GET', path: '/schedule' }, { method: 'POST', path: /^\/(lineup|waivers|trends|digest|retro|trade-finder|history)$/ }],
  evalAdapters: cardAdapters,
  actions: [runJob],
  workflows: {
    [LINEUP_WORKFLOW]: job(lineupFromEnv),
    [WAIVERS_WORKFLOW]: job(waiversFromEnv),
    [FINDER_WORKFLOW]: job(finderFromEnv),
    [TRENDS_WORKFLOW]: job(trendsFromEnv),
    [HISTORY_WORKFLOW]: job(historyFromEnv),
    [DIGEST_WORKFLOW]: job(digestFromEnv),
    [RETRO_WORKFLOW]: job(retroFromEnv),
  },
};
