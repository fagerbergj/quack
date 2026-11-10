import { resolve } from 'node:path';
import { mergeConfig } from 'vitest/config';
import web from '../../packages/web/vite.config.ts';
import { mutant } from './vitest.config.ts';

// The web package's own config, rooted at packages/web, plus the mutant loader.
export default mergeConfig(web, { root: resolve(import.meta.dirname, '../../packages/web'), plugins: [mutant], test: { fsModuleCache: false } });
