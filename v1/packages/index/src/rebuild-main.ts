// Usage: QUACK_INDEX_DATABASE_URL=... QUACK_DATA_DIR=... node rebuild-main.ts   (stop the distributor first)
import pg from 'pg';
import { rebuild } from './rebuild.ts';
import { createStore } from './store.ts';

const required = (name: string) => process.env[name] || (() => { throw new Error(`${name} is required`); })();
const pool = new pg.Pool({ connectionString: required('QUACK_INDEX_DATABASE_URL') });
try {
  console.log(`replayed ${await rebuild(createStore(pool), required('QUACK_DATA_DIR'))} sessions`);
} finally {
  await pool.end();
}
