import assert from 'node:assert/strict';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import pg from 'pg';
import type { WaitingStore } from '../src/queue.ts';
import { postgresWaiting, sqliteWaiting } from '../src/waiting-store.ts';

const url = process.env.QUACK_TEST_DATABASE_URL;
const cleanups: Array<() => Promise<unknown>> = [];
after(async () => { for (const c of cleanups.reverse()) await c(); });

// Each open returns a store over the same data, so a test can reopen it as a restart would.
type Backend = { name: string; skip: string | false; fresh: () => Promise<() => WaitingStore> };

const backends: Backend[] = [
  { name: 'sqlite', skip: false, fresh: async () => {
    const file = join(mkdtempSync(join(tmpdir(), 'gh-wait-')), 'github.db');
    return () => sqliteWaiting(file);
  } },
  { name: 'postgres', skip: !url && 'QUACK_TEST_DATABASE_URL is not set', fresh: async () => {
    // Each test gets its own schema so runs never see each other's rows.
    const schema = `t_${Math.random().toString(36).slice(2, 10)}`;
    const admin = new pg.Pool({ connectionString: url, max: 1 });
    await admin.query(`create schema ${schema}`);
    cleanups.push(async () => { await admin.query(`drop schema ${schema} cascade`); await admin.end(); });
    return () => postgresWaiting(new pg.Pool({ connectionString: url, options: `-c search_path=${schema}` }));
  } },
];

for (const { name, skip, fresh } of backends) {
  test(`${name}: put returns what it replaced, list holds one item per key, removeIf keeps a newer item, and a reopen sees it all`, { skip }, async () => {
    const open = await fresh();
    const store = open();
    assert.equal(await store.put('a', '{"n":1}'), undefined);
    assert.equal(await store.put('a', '{"n":2}'), '{"n":1}');
    assert.equal(await store.put('b', '{"n":3}'), undefined);
    assert.deepEqual(await store.list(), [{ key: 'a', item: '{"n":2}' }, { key: 'b', item: '{"n":3}' }]);
    await store.removeIf('a', '{"n":1}');
    assert.equal((await store.list()).length, 2, 'an item replaced during an attempt is kept');
    await store.removeIf('a', '{"n":2}');
    await store.close();
    const reopened = open();
    assert.deepEqual(await reopened.list(), [{ key: 'b', item: '{"n":3}' }]);
    await reopened.close();
  });
}
