import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { BatchError, createCatalog, fileJournal, sqliteCatalog, type CatalogEvent } from '../src/index.ts';

const dir = await mkdtemp(join(tmpdir(), 'quack-catalog-core-'));
const store = sqliteCatalog(join(dir, 'c.sqlite'), { currency: 'USD' });
after(async () => { await store.close(); await rm(dir, { recursive: true, force: true }); });

const title = (seq: number, t: string): CatalogEvent => ({ seq, id: `title:${seq}`, at: seq, type: 'title.set', data: { title: t } });

test('a batch whose seqs do not strictly increase is refused whole, never applied in part', async () => {
  const catalog = createCatalog({ store, journal: fileJournal(join(dir, 'journal')), log: () => {} });
  await catalog.publish('k', [{ seq: 1, id: 'session', at: 0, type: 'session.created', data: { agent: 'a', createdAt: 0 } }]);
  for (const seqs of [[2, 4, 3], [2, 2], [0, 2]]) {
    await assert.rejects(catalog.publish('k', seqs.map((n) => title(n, `t${n}`))), BatchError, String(seqs));
    assert.deepEqual([await catalog.watermark('k'), (await catalog.getSession('k'))?.title], [1, undefined], `${seqs} applied nothing`);
  }
  assert.deepEqual(await catalog.publish('k', [title(2, 'two'), title(3, 'three')]), { acked: 3 });
});

test('every committed write reaches a subscriber by its key: a channel batch, an answered ask, a stop; a refused batch does not', async () => {
  const catalog = createCatalog({ store, journal: fileJournal(join(dir, 'journal')), log: () => {} });
  const heard: string[] = [];
  const unsubscribe = catalog.subscribe((key) => heard.push(key));
  await catalog.publish('w', [{ seq: 1, id: 'w-session', at: 0, type: 'session.created', data: { agent: 'a', createdAt: 0 } }, { seq: 2, id: 'w-ask', at: 1, type: 'ask.opened', data: { id: 'q', type: 'question', createdAt: 1 } }]);
  await assert.rejects(catalog.publish('w', [title(4, 'x'), title(3, 'y')]), BatchError);
  await catalog.resolve('w', 'q', { by: { kind: 'tailnet' }, choice: 'yes' });
  await catalog.stopped('w');
  assert.deepEqual(heard, ['w', 'w', 'w']);
  unsubscribe();
  await catalog.rename('w', 'later');
  assert.equal(heard.length, 3, 'an unsubscribed listener hears nothing');
});
