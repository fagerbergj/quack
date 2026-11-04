import assert from 'node:assert/strict';
import { test } from 'node:test';
import { checkedSettings } from '@quack/sdk';
import { settingsProblems, type SleeperSettings } from '../src/settings.ts';

test('the sleeper service takes its league and owner from its settings and refuses anything else', () => {
  assert.deepEqual(checkedSettings<SleeperSettings>('sleeper', { league: '1', owner: 'me' }, settingsProblems), { league: '1', owner: 'me' });
  assert.equal(checkedSettings<SleeperSettings>('sleeper', { league: '1' }, settingsProblems).owner, undefined);
  assert.throws(() => checkedSettings('sleeper', undefined, settingsProblems), /league must be a non-empty string/);
  assert.throws(() => checkedSettings('sleeper', { league: '1', owner: '' }, settingsProblems), /owner must be a non-empty string/);
  assert.throws(() => checkedSettings('sleeper', { league: '1', leauge: '2' }, settingsProblems), /leauge is not a sleeper setting/);
});
