import assert from 'node:assert/strict';
import { test } from 'node:test';
import { transcriptKind } from '../src/ingest/transcript.ts';

test('a transcript summarizes as its topic and page count', () => {
  const t = (pages: number) => JSON.stringify({ document: { name: 'Notes' }, topic: 'Session 12 prep', pages: Array.from({ length: pages }, () => ({})) });
  assert.equal(transcriptKind.summarize!(t(3)), 'Session 12 prep · 3 pages');
  assert.equal(transcriptKind.summarize!(t(1)), 'Session 12 prep · 1 page');
});
