import type { ExtensionManifest } from '@quack/sdk';
import type { RubricFor } from './agent-bundle.ts';

// The rubric an agent runs with is the one its selected extensions' eval adapters judge by.
// shortcut: an extension with several adapters yields the first; key rubrics by agent when a second kind needs showing.
export const rubricFrom = (manifests: ExtensionManifest[]): RubricFor => (names) =>
  manifests.filter((m) => names.includes(m.name)).flatMap((m) => m.evalAdapters ?? []).find((a) => a.judge)?.judge?.rubric;
