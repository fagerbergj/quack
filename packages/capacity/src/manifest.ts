import { existsSync, renameSync } from 'node:fs';
import { dirname, join } from 'node:path';
import type { ExtensionManifest } from '@quack/sdk';
import { capacityLimits } from './limits.ts';
import { createCapacityService } from './service.ts';
import { capacityFromEnv } from './session.ts';

// shortcut: takes over the ledger kept as slots.db before the rename, once; delete one release after it ships.
export function adoptSlotsLedger(dbPath: string): void {
  const old = join(dirname(dbPath), 'slots.db');
  if (existsSync(dbPath) || !existsSync(old)) return;
  for (const suffix of ['-journal', '-wal', '-shm', '']) if (existsSync(old + suffix)) renameSync(old + suffix, dbPath + suffix);
}

export const manifest: ExtensionManifest = {
  name: 'capacity',
  deployment: true,
  session: () => capacityFromEnv(),
  app: ({ dbPath, deployment }) => (adoptSlotsLedger(dbPath), createCapacityService({ dbPath, capacity: capacityLimits({ providers: deployment.providers ?? {} }) })),
};
