import type { ExtensionManifest } from '@quack/sdk';
import { configProblems, judgeSession, type JudgeConfig } from './extension.ts';

// An agent that selects the judge has its registered kinds judged on every seal; one that does not seals them unchecked.
export const manifest: ExtensionManifest = {
  name: 'judge',
  validate: configProblems,
  session: (config) => judgeSession((config ?? {}) as JudgeConfig),
};
