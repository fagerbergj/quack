import { CodingTools } from '@earendil-works/pi-durable/tools';
import type { ExtensionManifest } from '@quack/sdk';

export const manifest: ExtensionManifest = { name: 'coding', session: () => [CodingTools] };
