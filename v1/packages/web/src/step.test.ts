import { expect, test } from 'vitest';
import { stepLabel } from './step.ts';
import { applyEvents, emptyView } from './view.ts';

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

test('the view holds the in-flight tool from its start until it ends', () => {
  const started = applyEvents(emptyView, [{ type: 'tool_execution_start', toolCallId: 't', toolName: 'bash', args: {} }] as never);
  expect(started.tool?.name).toBe('bash');
  expect(applyEvents(started, [{ type: 'tool_execution_end', toolCallId: 't' }] as never).tool).toBeUndefined();
});
