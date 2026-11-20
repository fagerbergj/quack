import type { ExtensionManifest } from '@quack/sdk';
import { tracesFromEnv } from './session.ts';

export const manifest: ExtensionManifest = {
  name: 'traces',
  deployment: true,
  env: ['LANGFUSE_BASE_URL', 'LANGFUSE_PUBLIC_KEY', 'LANGFUSE_SECRET_KEY'],
  proxiedServices: [{ name: 'LANGFUSE', urlEnv: 'LANGFUSE_BASE_URL', secretEnv: 'LANGFUSE_SECRET_KEY', userEnv: 'LANGFUSE_PUBLIC_KEY', userPlaceholder: 'session', auth: 'basic' }],
  session: () => tracesFromEnv(),
};
