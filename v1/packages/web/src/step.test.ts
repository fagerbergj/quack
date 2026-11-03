import { expect, test } from 'vitest';
import { stepLabel } from './step.ts';
import { applyView, emptyView } from './view.ts';

const tool = (name: string, args: unknown = {}) => ({ tool: { name, args } });

test('a running tool reads as plain words, never its name or args', () => {
  expect(stepLabel(tool('checkout_pr', { number: 4 }))).toBe('Checking out the latest commit');
  expect(stepLabel(tool('read', { path: 'v1/packages/web/src/SessionView.tsx' }))).toBe('Reading SessionView.tsx');
  expect(stepLabel(tool('read', {}))).toBe('Reading a file');
  expect(stepLabel(tool('bash', { command: 'rm -rf x' }))).toBe('Running a command');
  expect(stepLabel(tool('submit_review'))).toBe('Waiting for the double-check');
});

test('an unknown tool falls back to Working', () => {
  expect(stepLabel(tool('frobnicate', { secret: 1 }))).toBe('Working');
});

test('without a tool the label follows what is being generated', () => {
  const partial = (type: string) => ({ partial: { content: [{ type }] } }) as never;
  expect(stepLabel(partial('thinking'))).toBe('Thinking');
  expect(stepLabel(partial('text'))).toBe('Writing');
  expect(stepLabel({})).toBe('Working');
});

test('the view holds the in-flight tool, with its call arguments, from its start until it is done', () => {
  const call = { type: 'toolCall', id: 't', name: 'bash', arguments: { command: 'ls' } };
  const items = [{ kind: 'entry', entry: { id: 1, kind: 'pi.assistant', model: [{ role: 'assistant', content: [call] }] } }];
  const slot = (status: string) => ({ callId: 't', name: 'bash', status });
  const started = applyView(emptyView, { type: 'view', items, live: { run: {}, tools: [slot('running')] } } as never);
  expect(started.tool).toEqual({ name: 'bash', args: { command: 'ls' } });
  expect(applyView(started, { type: 'view', live: { run: {}, tools: [slot('done')] } } as never).tool).toBeUndefined();
});
