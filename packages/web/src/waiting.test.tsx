import { render, screen, within } from '@testing-library/react';
import { expect, test } from 'vitest';
import { App } from './App.tsx';
import { FakeSocket, routes, sess, snapshot, stubViewport, useHarness } from './test-harness.ts';

useHarness(() => ({ 'GET /sessions': { sessions: [] }, 'GET /agents': { agents: [] }, 'GET /artifacts': { artifacts: [] } }));

test('a running session queued for a model slot reads "Waiting…" in the list, naming the provider and how many are ahead for assistive tech; one that is generating does not', async () => {
  routes['GET /sessions'] = { sessions: [
    sess('gh:queued', { title: 'Queued', state: 'running', waiting: { since: 1, ahead: 2, provider: 'local' } }),
    sess('gh:going', { title: 'Going', state: 'running' }),
  ] };
  stubViewport(false);
  location.hash = '#/sessions';
  render(<App />);
  const running = await screen.findByRole('region', { name: 'Running' });
  const queued = within(running).getByRole('link', { name: /Queued/ });
  expect(queued).toHaveTextContent('Waiting…');
  expect(queued).toHaveTextContent('Waiting for local capacity · 2 ahead');
  expect(queued).not.toHaveTextContent(/\d+ s\b/);
  expect(within(queued).getByTitle('Waiting for local capacity · 2 ahead')).toBeInTheDocument();
  expect(within(running).getByRole('link', { name: /Going/ })).not.toHaveTextContent(/Waiting/);
});

test('the session view says "Waiting…" instead of what it is generating, and drops it when the wait ends', async () => {
  location.hash = '#/s/ui%3Au1';
  render(<App />);
  const ws = FakeSocket.all[0]!;
  ws.open();
  ws.frame(snapshot({ run: true }));
  expect(screen.queryByText('Waiting…')).not.toBeInTheDocument();
  ws.frame({ type: 'waiting', waiting: { since: 60_000, ahead: 2, provider: 'local' } });
  expect(screen.getByText('Waiting…')).toBeInTheDocument();
  expect(screen.getByText('Waiting for local capacity · 2 ahead')).toHaveClass('sr-only');
  expect(screen.getByTitle('Waiting for local capacity · 2 ahead')).toBeInTheDocument();
  expect(screen.queryByText(/\d+ s\b/)).not.toBeInTheDocument();
  ws.frame({ type: 'waiting', waiting: null });
  expect(screen.queryByText('Waiting…')).not.toBeInTheDocument();
});

test('a double-check queued for a model slot keeps its check rows under "Waiting…", the provider and how many are ahead in its tooltip and label', async () => {
  location.hash = '#/s/ui%3Au1';
  render(<App />);
  const ws = FakeSocket.all[0]!;
  ws.open();
  const checks = [{ criterion: 'c', id: 'sup', label: 'A claim is not stated', weight: 1 }, { criterion: 'c', id: 'cites', label: 'Cites no source', weight: 'gate', met: 1 }];
  ws.frame(snapshot({ run: true, tools: [{ callId: 'b', name: 'submit_report', status: 'running', details: { rubric: 'r', checks } }] }));
  ws.frame({ type: 'waiting', waiting: { since: 60_000, ahead: 3, provider: 'local' } });
  expect(screen.getByText('Waiting for local capacity · 3 ahead')).toHaveClass('sr-only');
  expect(screen.getByText('Waiting…')).toBeInTheDocument();
  expect(within(screen.getByRole('list', { name: 'Checks running' })).getByText('A claim is not stated')).toBeInTheDocument();
  expect(screen.getByText(/^Done ·/)).toBeInTheDocument();
});

test('a run parked on a failing model endpoint reads "Waiting…", with the provider and its last error for assistive tech and the tooltip', async () => {
  location.hash = '#/s/ui%3Au1';
  render(<App />);
  const ws = FakeSocket.all[0]!;
  ws.open();
  ws.frame(snapshot({ run: true }));
  ws.frame({ type: 'waiting', waiting: { since: 60_000, ahead: 0, provider: 'local', error: '502 peer proxy error: connect: no route to host' } });
  expect(screen.getByText('Waiting…')).toBeInTheDocument();
  expect(screen.getByTitle('Waiting for local to answer · 502 peer proxy error: connect: no route to host')).toBeInTheDocument();
  expect(screen.queryByText(/capacity/)).not.toBeInTheDocument();
});
