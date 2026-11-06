import { render, screen } from '@testing-library/react';
import { expect, test } from 'vitest';
import { App } from './App.tsx';
import { FakeSocket, fetchMock, snapshot, useHarness, view } from './test-harness.ts';

useHarness(() => ({ 'GET /sessions': { sessions: [] }, 'GET /agents': { agents: [] } }));

const report = (version: number) => ({ kind: 'research.report', version, hash: `h${version}`, content: 'body', sealedAt: version, verdict: null });

test('sealed versions arrive over the socket; tool starts and ends fetch nothing', async () => {
  location.hash = '#/s/ui%3Au1';
  render(<App />);
  const ws = FakeSocket.all[0]!;
  ws.open();
  ws.frame(snapshot({ run: true }));
  for (let i = 0; i < 20; i++) ws.frame(view({ run: true, tools: [{ callId: `t${i}`, name: 'bash', status: i % 2 ? 'done' : 'running' }] }));
  ws.frame({ type: 'artifacts', artifacts: [report(1)] });
  expect(await screen.findByRole('button', { name: 'Open Report' })).toBeInTheDocument();
  ws.frame({ type: 'artifacts', artifacts: [report(1), report(2)] });
  expect(screen.getByRole('button', { name: 'Open Report version 2' })).toBeInTheDocument();
  expect(fetchMock.mock.calls.map(([u]) => String(u)).filter((u) => u.startsWith('sessions/ui%3Au1/') && !u.endsWith('/meta'))).toEqual([]);
});
