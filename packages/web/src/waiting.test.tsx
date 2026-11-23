import { render, screen, within } from '@testing-library/react';
import { expect, test } from 'vitest';
import { App } from './App.tsx';
import { FakeSocket, routes, sess, snapshot, stubViewport, useHarness } from './test-harness.ts';

useHarness(() => ({ 'GET /sessions': { sessions: [] }, 'GET /agents': { agents: [] }, 'GET /artifacts': { artifacts: [] } }));

test('a running session queued for a model slot reads "Waiting…" in the list, naming the provider for assistive tech; one that is generating does not', async () => {
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
  expect(queued).toHaveTextContent('Waiting for local capacity');
  expect(queued).not.toHaveTextContent(/ahead|\d+ s\b/);
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
  expect(screen.getByText('Waiting for local capacity')).toHaveClass('sr-only');
  expect(screen.getByTitle('Waiting for local capacity · 2 ahead')).toBeInTheDocument();
  expect(screen.queryByText(/ahead|\d+ s\b/)).not.toBeInTheDocument();
  ws.frame({ type: 'waiting', waiting: null });
  expect(screen.queryByText('Waiting…')).not.toBeInTheDocument();
});
