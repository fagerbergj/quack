import { CodingTools } from '@earendil-works/pi-durable/tools';
import type { Catalog } from '@quack/session';

// Extensions this runtime image carries, by the name agent definitions select them with (A8).
export const catalog: Catalog = {
  coding: () => [CodingTools],
};
