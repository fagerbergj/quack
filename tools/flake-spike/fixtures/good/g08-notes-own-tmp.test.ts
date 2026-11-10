import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { saveNote } from '../subjects/notes.ts';

const dir = mkdtempSync(join(tmpdir(), 'notes-'));
after(() => rmSync(dir, { recursive: true, force: true }));

test('a saved note is written as <slug>.md, and only .md files count as notes', () => {
  writeFileSync(join(dir, 'README.txt'), 'not a note');
  assert.equal(saveNote(dir, 'first', 'hello'), 1);
  assert.equal(saveNote(dir, 'second', 'again'), 2);
  assert.equal(readFileSync(join(dir, 'first.md'), 'utf8'), 'hello');
});
