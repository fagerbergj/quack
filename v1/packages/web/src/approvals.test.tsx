import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test } from 'vitest';
import { App } from './App.tsx';
import { FakeSocket, fetchMock, live, routes, snapshot, stubViewport, useHarness, view } from './test-harness.ts';

useHarness(() => ({ 'GET /sessions': { sessions: [] }, 'GET /agents': { agents: [] } }));

async function openSession() {
  location.hash = '#/s/ui%3Au1';
  render(<App />);
  const ws = FakeSocket.all[0]!;
  ws.open();
  return ws;
}

const lastPost = () => JSON.parse(fetchMock.mock.calls.filter(([, i]) => i?.method === 'POST').at(-1)![1]!.body as string);

test('approving posts the verdict and clears the item', async () => {
  const pending = { id: 'a/1', tool: 'bash', args: { cmd: 'rm x' }, requestedAt: 1 };
  (live['ui%3Au1'] ??= {}).approvals = [pending];
  routes['POST /sessions/ui%3Au1/approvals/a%2F1'] = { resolved: true };
  const ws = await openSession();
  ws.frame(snapshot());
  const approve = await screen.findByRole('button', { name: /^Approve/ });
  await userEvent.click(approve);
  await waitFor(() => expect(screen.queryByRole('button', { name: /^Approve/ })).toBeNull());
  const post = fetchMock.mock.calls.find(([, init]) => init?.method === 'POST')!;
  expect(JSON.parse(post[1]!.body as string)).toEqual({ decision: 'approve' });
});

test('an approval a gated tool commits after it is already running shows its card, with no refetch', async () => {
  const ws = await openSession();
  ws.frame(snapshot());
  ws.frame(view({ run: true, tools: [{ callId: 't', name: 'push', status: 'running' }] }));
  expect(screen.getByRole('status')).toHaveTextContent('Running');
  ws.frame({ type: 'standing', questions: [], failures: [], approvals: [{ id: 'b', tool: 'push', args: {}, requestedAt: 1 }] });
  expect(screen.getByRole('button', { name: /^Reject/ })).toBeInTheDocument();
  expect(screen.getByRole('status')).toHaveTextContent('Waiting on you');
  expect(fetchMock.mock.calls.filter(([u]) => /^sessions\/ui%3Au1\/(approvals|artifacts)/.test(String(u)))).toEqual([]);
});

test('a pending approval from the session socket shows with the reason field, and the rail lists it', async () => {
  live['ui%3Au1'] = { approvals: [{ id: 'a/1', tool: 'propose_contract', args: { summary: 'Ship the contract' }, requestedAt: 1 }] };
  routes['POST /sessions/ui%3Au1/approvals/a%2F1'] = { resolved: true };
  stubViewport(true, true);
  const ws = await openSession();
  ws.frame(snapshot());
  const card = await screen.findByRole('region', { name: 'Approval' });
  expect(within(card).getAllByText('Ship the contract').length).toBeGreaterThan(0);
  expect(screen.getAllByRole('region', { name: 'Approval' })).toHaveLength(1);
  expect(fetchMock.mock.calls.some(([u]) => String(u).startsWith('sessions/ui%3Au1/approvals'))).toBe(false);
  expect(within(screen.getByRole('complementary', { name: 'Context' })).getByRole('button', { name: 'Ship the contract' })).toBeInTheDocument();
  await userEvent.click(within(card).getByRole('button', { name: /^Reject/ }));
  await userEvent.type(screen.getByRole('textbox', { name: 'Reason' }), 'no');
  await userEvent.click(screen.getByRole('button', { name: /^Confirm reject/ }));
  expect(lastPost()).toEqual({ decision: 'reject', reason: 'no' });
  await waitFor(() => expect(screen.queryByRole('region', { name: 'Approval' })).toBeNull());
});
