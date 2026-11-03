// Usage: QUACK_DATA_DIR=... QUACK_CONFIG=deployment.json (prices tokens at current rates) [QUACK_INDEX_DATABASE_URL=...] node rebuild-main.ts   (stop the distributor first)
import { readFileSync } from 'node:fs';
import { parseDeployment, priceOf } from '@quack/session';
import { openStore } from './open.ts';
import { rebuild } from './rebuild.ts';

const dataDir = process.env.QUACK_DATA_DIR;
if (!dataDir) throw new Error('QUACK_DATA_DIR is required');
if (!process.env.QUACK_CONFIG) throw new Error('QUACK_CONFIG is required');
const prices = priceOf(parseDeployment(JSON.parse(readFileSync(process.env.QUACK_CONFIG, 'utf8'))));
const { store, close } = openStore({ QUACK_INDEX_DATABASE_URL: process.env.QUACK_INDEX_DATABASE_URL, QUACK_DATA_DIR: dataDir });
try {
  console.log(`replayed ${await rebuild(store, dataDir, Date.now, prices)} sessions`);
} finally {
  await close();
}
