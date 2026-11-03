import assert from 'node:assert/strict';
import { test } from 'node:test';
import { triggerFor } from '../src/index.ts';

const repository = { name: 'widgets', owner: { login: 'acme' } };
const comment = (body: string, association = 'MEMBER', pull = true) => ({ action: 'created', issue: { number: 7, title: 'Fix the cache', ...(pull ? { pull_request: {} } : {}) }, comment: { body, author_association: association, user: { login: 'someone' } }, repository });
const fix = (body: string, association?: string, pull?: boolean) => triggerFor('issue_comment', comment(body, association, pull), 'quack:review', 'quack-app');

test('a trusted /fix on a PR starts the implementer in the PR session, with the text after it as the instruction', () => {
  const t = fix('/fix the flaky cache test\nand the lint error')!;
  assert.equal(t.role, 'implementer');
  assert.equal(t.sessionKey, 'github:acme/widgets#7');
  assert.equal(t.followUp, undefined);
  assert.match(t.input, /Fix pull request acme\/widgets#7 \(\/fix by someone\)/);
  assert.match(t.input, /Instruction: the flaky cache test\nand the lint error/);
});

test('a bare /fix asks for CI and review findings', () => {
  assert.match(fix(' /fix ')!.input, /Instruction: \(none; fix failing CI and open review findings\)/);
});

test('/fix from an untrusted author, on an issue, or glued to other text starts nothing', () => {
  assert.equal(fix('/fix it', 'CONTRIBUTOR'), undefined);
  assert.equal(fix('/fix it', 'MEMBER', false), undefined);
  assert.equal(fix('/fixme'), undefined);
  assert.equal(fix('please /fix it'), undefined);
});
