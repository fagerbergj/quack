import type { ExtensionManifest } from '@quack/sdk';
import { HISTORY_WORKFLOW, historyFromEnv } from './history/index.ts';
import { LINEUP_WORKFLOW, lineupFromEnv } from './lineup/index.ts';
import { DIGEST_WORKFLOW, digestFromEnv, RETRO_WORKFLOW, retroFromEnv } from './report/index.ts';
import { FINDER_WORKFLOW, finderFromEnv, TRADE_WORKFLOW, tradeFromEnv } from './trades/index.ts';
import { TRENDS_WORKFLOW, trendsFromEnv } from './trends/index.ts';
import { WAIVERS_WORKFLOW, waiversFromEnv } from './waivers/index.ts';

const env = ['QUACK_SLEEPER_URL', 'QUACK_SLEEPER_TOKEN', 'QUACK_EXA_API_KEY'];

/** Sleeper contributes workflows only; an agent binds one through the workflows extension. */
export const manifest: ExtensionManifest = {
  name: 'sleeper',
  workflows: {
    [LINEUP_WORKFLOW]: { build: () => lineupFromEnv(), env },
    [WAIVERS_WORKFLOW]: { build: () => waiversFromEnv(), env },
    [FINDER_WORKFLOW]: { build: () => finderFromEnv(), env },
    [TRADE_WORKFLOW]: { build: () => tradeFromEnv(), env },
    [TRENDS_WORKFLOW]: { build: () => trendsFromEnv(), env },
    [HISTORY_WORKFLOW]: { build: () => historyFromEnv(), env },
    [DIGEST_WORKFLOW]: { build: () => digestFromEnv(), env },
    [RETRO_WORKFLOW]: { build: () => retroFromEnv(), env },
  },
};
