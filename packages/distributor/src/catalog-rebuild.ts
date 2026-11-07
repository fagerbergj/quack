// Usage: QUACK_DATA_DIR=... QUACK_CONFIG=deployment.json node packages/distributor/src/catalog-rebuild.ts   (stop the distributor first: a log has one owner)
// The first-boot backfill marks itself done even when single sessions fail to rebuild (each is logged); this tool recovers them.
import { readDeploymentFile } from '@quack/sdk';
import { catalogStore, openCatalog } from '@quack/catalog';
import { parseDeployment, priceOf } from '@quack/session';
import { rebuildCatalog } from './catalog-sync.ts';

const dataDir = process.env.QUACK_DATA_DIR;
if (!dataDir) throw new Error('QUACK_DATA_DIR is required');
const deployment = readDeploymentFile();
const catalog = openCatalog({ dataDir, store: catalogStore(deployment) });
try {
  await catalog.ensure();
  console.log(`rebuilt ${await rebuildCatalog(catalog, dataDir, priceOf(parseDeployment(deployment)))} sessions`);
} finally {
  await catalog.close();
}
