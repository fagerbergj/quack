import type { ExtensionManifest } from '@quack/sdk';
import { withChat } from './chat.ts';
import { HISTORY_WORKFLOW, historyFromEnv } from './history/index.ts';
import { LINEUP_WORKFLOW, lineupFromEnv } from './lineup/index.ts';
import { DIGEST_WORKFLOW, digestFromEnv, RETRO_WORKFLOW, retroFromEnv } from './report/index.ts';
import { FINDER_WORKFLOW, finderFromEnv, TRADE_WORKFLOW, tradeFromEnv } from './trades/index.ts';
import { TRENDS_WORKFLOW, trendsFromEnv } from './trends/index.ts';
import { waiversEvalAdapter, WAIVERS_WORKFLOW, waiversFromEnv } from './waivers/index.ts';

const env = ['QUACK_SLEEPER_URL', 'QUACK_SLEEPER_TOKEN', 'QUACK_EXA_API_KEY'];

/** Sleeper contributes workflows and an eval adapter for its waivers card; an agent binds one through the workflows extension. */
export const manifest: ExtensionManifest = {
  name: 'sleeper',
  evalAdapters: [waiversEvalAdapter],
  workflows: {
    [LINEUP_WORKFLOW]: { build: () => withChat(lineupFromEnv()), env },
    [WAIVERS_WORKFLOW]: { build: () => withChat(waiversFromEnv()), env },
    [FINDER_WORKFLOW]: { build: () => withChat(finderFromEnv()), env },
    [TRADE_WORKFLOW]: { build: () => withChat(tradeFromEnv()), env },
    [TRENDS_WORKFLOW]: { build: () => withChat(trendsFromEnv()), env },
    [HISTORY_WORKFLOW]: { build: () => withChat(historyFromEnv()), env },
    [DIGEST_WORKFLOW]: { build: () => withChat(digestFromEnv()), env },
    [RETRO_WORKFLOW]: { build: () => withChat(retroFromEnv()), env },
  },
};
