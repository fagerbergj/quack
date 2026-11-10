import assert from 'node:assert/strict';
import { mkdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { saveNote } from '../subjects/notes.ts';

const dir = join(tmpdir(), 'flake-spike-notes');

test('the first note saved into an empty dir is its only note', () => {
  mkdirSync(dir, { recursive: true });
  try {
    assert.equal(saveNote(dir, `n${process.pid}`, 'hello'), 1);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
