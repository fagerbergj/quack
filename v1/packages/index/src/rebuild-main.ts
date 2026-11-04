// Usage: QUACK_DATA_DIR=... QUACK_CONFIG=deployment.json (prices tokens at current rates) node rebuild-main.ts   (stop the distributor first)
import { readDeploymentFile, storeFor } from '@quack/sdk';
import { parseDeployment, priceOf } from '@quack/session';
import { openStore } from './open.ts';
import { rebuild } from './rebuild.ts';

const dataDir = process.env.QUACK_DATA_DIR;
if (!dataDir) throw new Error('QUACK_DATA_DIR is required');
const deployment = readDeploymentFile();
const prices = priceOf(parseDeployment(deployment));
const { store, close } = openStore({ dataDir, store: storeFor(deployment, deployment.stores?.index && 'index') });
try {
  console.log(`replayed ${await rebuild(store, dataDir, Date.now, prices)} sessions`);
} finally {
  await close();
}
