import type { ExtensionManifest } from '@quack/sdk';
import { tracesFromEnv } from './session.ts';

export const manifest: ExtensionManifest = { name: 'traces', deployment: true, env: ['LANGFUSE_BASE_URL', 'LANGFUSE_PUBLIC_KEY', 'LANGFUSE_SECRET_KEY'], session: () => tracesFromEnv() };
