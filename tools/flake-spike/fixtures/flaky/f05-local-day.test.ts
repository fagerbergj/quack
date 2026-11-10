import assert from 'node:assert/strict';
import { test } from 'node:test';
import { day } from '../subjects/dates.ts';

test("an event with no time falls on today's date", () => {
  assert.equal(day(), new Date().toISOString().slice(0, 10));
});
