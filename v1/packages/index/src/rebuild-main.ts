// Usage: QUACK_DATA_DIR=... [QUACK_INDEX_DATABASE_URL=...] node rebuild-main.ts   (stop the distributor first)
import { openStore } from './open.ts';
import { rebuild } from './rebuild.ts';

const dataDir = process.env.QUACK_DATA_DIR;
if (!dataDir) throw new Error('QUACK_DATA_DIR is required');
const { store, close } = openStore({ QUACK_INDEX_DATABASE_URL: process.env.QUACK_INDEX_DATABASE_URL, QUACK_DATA_DIR: dataDir });
try {
  console.log(`replayed ${await rebuild(store, dataDir)} sessions`);
} finally {
  await close();
}
