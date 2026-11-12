import type { ExtensionManifest } from '@quack/sdk';
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
    [LINEUP_WORKFLOW]: { build: (config) => withChat(lineupFromEnv(process.env, config as Parameters<typeof lineupFromEnv>[1])), env },
    [WAIVERS_WORKFLOW]: { build: (config) => withChat(waiversFromEnv(process.env, config as Parameters<typeof waiversFromEnv>[1])), env },
    [FINDER_WORKFLOW]: { build: (config) => withChat(finderFromEnv(process.env, config as Parameters<typeof finderFromEnv>[1])), env },
    [TRENDS_WORKFLOW]: { build: (config) => withChat(trendsFromEnv(process.env, config as Parameters<typeof trendsFromEnv>[1])), env },
    [HISTORY_WORKFLOW]: { build: () => withChat(historyFromEnv()), env },
    [DIGEST_WORKFLOW]: { build: (config) => withChat(digestFromEnv(process.env, config as Parameters<typeof digestFromEnv>[1])), env },
    [RETRO_WORKFLOW]: { build: (config) => withChat(retroFromEnv(process.env, config as Parameters<typeof retroFromEnv>[1])), env },
  },
};
