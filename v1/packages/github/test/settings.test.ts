import assert from 'node:assert/strict';
import { test } from 'node:test';
import { checkedSettings } from '@quack/sdk';
import { settingsProblems, type GithubSettings } from '../src/settings.ts';

test('github settings take an implementer agent and an issue base, and refuse anything else', () => {
  assert.deepEqual(checkedSettings<GithubSettings>('github', { implementerAgent: 'implementer', issueBase: { 'acme/widgets': 'v1' } }, settingsProblems), { implementerAgent: 'implementer', issueBase: { 'acme/widgets': 'v1' } });
  assert.equal(checkedSettings<GithubSettings>('github', undefined, settingsProblems), undefined);
  assert.throws(() => checkedSettings('github', { issueBase: 'v1' }, settingsProblems), /issueBase must map owner\/repo to a branch name/);
  assert.throws(() => checkedSettings('github', { issueBase: { widgets: 'v1' } }, settingsProblems), /issueBase must map/);
  assert.throws(() => checkedSettings('github', { implementerAgent: 3 }, settingsProblems), /implementerAgent must be an agent name/);
  assert.throws(() => checkedSettings('github', { issuebase: 'v1' }, settingsProblems), /issuebase is not a github setting/);
});
