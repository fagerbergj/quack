import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test } from 'vitest';
import { App } from './App.tsx';
import { Fail, FakeSocket, fetchMock, routes, snapshot, stubViewport, useHarness } from './test-harness.ts';

const MODELS = { models: [{ name: 'local/flash', contextWindow: 262144 }, { name: 'local/big', contextWindow: 131072, images: true }], defaultModel: 'local/flash' };

useHarness(() => ({ 'GET /sessions': { sessions: [] }, 'GET /agents': { agents: [] }, 'GET /asks': { asks: [] }, 'GET /models': MODELS }));

const patches = (path: string) => fetchMock.mock.calls.filter(([url, init]) => url === path && init?.method === 'PATCH').map(([, init]) => JSON.parse(String(init!.body)));

function openSession() {
  location.hash = '#/s/ui%3Au1';
  render(<App />);
  const ws = FakeSocket.all[0]!;
  ws.open();
  ws.frame(snapshot());
  return ws;
}

test('the composer picks the session\'s model: Default names the default in its description, a pick is sent as the session\'s model', async () => {
  routes['GET /sessions/ui%3Au1'] = { key: 'ui:u1', resolvedModel: 'local/flash' };
  routes['PATCH /sessions/ui%3Au1'] = { key: 'ui:u1', model: 'local/big', resolvedModel: 'local/big' };
  openSession();
  const picker = await screen.findByRole('combobox', { name: 'Model' });
  expect(picker).toHaveValue('');
  expect(within(picker).getAllByRole('option').map((o) => o.textContent)).toEqual(['Default', 'flash', 'big']);
  expect(picker).toHaveAccessibleDescription('Model for the next run: local/flash, 256k context (default)');
  await userEvent.selectOptions(picker, 'local/big');
  await waitFor(() => expect(picker).toHaveValue('local/big'));
  expect(patches('sessions/ui%3Au1')).toEqual([{ model: 'local/big' }]);
  expect(picker).toHaveAccessibleDescription('Model for the next run: local/big, 128k context');
});

test('the composer shows the session\'s own pick, and a refused pick keeps it and says why', async () => {
  routes['GET /sessions/ui%3Au1'] = { key: 'ui:u1', model: 'local/big', resolvedModel: 'local/big' };
  routes['PATCH /sessions/ui%3Au1'] = new Fail(400, { detail: 'model local/flash is not available; available: local/big' });
  openSession();
  const picker = await screen.findByRole('combobox', { name: 'Model' });
  await waitFor(() => expect(picker).toHaveValue('local/big'));
  await userEvent.selectOptions(picker, 'local/flash');
  await waitFor(() => expect(picker).toHaveAccessibleDescription('model local/flash is not available; available: local/big'));
  expect(picker).toHaveValue('local/big');
  expect(picker).toHaveAttribute('aria-invalid', 'true');
});

test('a session without a pick whose agent names a model is described as running the agent\'s model', async () => {
  routes['GET /sessions/ui%3Au1'] = { key: 'ui:u1', resolvedModel: 'local/big' };
  openSession();
  const picker = await screen.findByRole('combobox', { name: 'Model' });
  await waitFor(() => expect(picker).toHaveAccessibleDescription('Model for the next run: local/big, 128k context (default)'));
  expect(picker).toHaveValue('');
});

test('Enter on the focused picker never sends the draft', async () => {
  const ws = openSession();
  await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'half a thought');
  const picker = await screen.findByRole('combobox', { name: 'Model' });
  picker.focus();
  // A browser's implicit submission is Enter's default action on a form control; false means the picker cancelled it.
  expect(fireEvent.keyDown(picker, { key: 'Enter' })).toBe(false);
  await userEvent.keyboard('{Enter}');
  expect(ws.sent).toEqual([]);
  expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue('half a thought');
});

test('two providers offering one id show their provider, so no two options read alike', async () => {
  routes['GET /models'] = { models: [{ name: 'local/m', contextWindow: 4096 }, { name: 'hosted/m', contextWindow: 4096 }, { name: 'local/solo', contextWindow: 4096 }] };
  openSession();
  const picker = await screen.findByRole('combobox', { name: 'Model' });
  expect(within(picker).getAllByRole('option').map((o) => o.textContent)).toEqual(['Default', 'local/m', 'hosted/m', 'solo']);
});

test('with no models listed the composer shows no picker', async () => {
  routes['GET /models'] = { models: [] };
  openSession();
  await screen.findByRole('textbox', { name: 'Message' });
  expect(screen.queryByRole('combobox', { name: 'Model' })).toBeNull();
});

test('the Settings page sets the default model on the server, and clearing it goes back to the server default', async () => {
  routes['PATCH /settings'] = { defaultModel: 'local/big' };
  location.hash = '#/settings';
  render(<App />);
  const select = await screen.findByRole('combobox', { name: /Default for new runs/ });
  await waitFor(() => expect(within(select).getAllByRole('option').map((o) => o.textContent)).toEqual(['Server default (local/flash)', 'local/flash', 'local/big']));
  routes['GET /settings'] = { defaultModel: 'local/big' };
  routes['GET /models'] = { ...MODELS, defaultModel: 'local/big' };
  await userEvent.selectOptions(select, 'local/big');
  await waitFor(() => expect(select).toHaveValue('local/big'));
  routes['PATCH /settings'] = {};
  routes['GET /settings'] = {};
  await userEvent.selectOptions(select, '');
  await waitFor(() => expect(select).toHaveValue(''));
  expect(patches('settings')).toEqual([{ defaultModel: 'local/big' }, { defaultModel: null }]);
});

test('the Settings page names a refused default', async () => {
  routes['PATCH /settings'] = new Fail(400, { detail: 'model local/big is not available; available: local/flash' });
  location.hash = '#/settings';
  render(<App />);
  const select = await screen.findByRole('combobox', { name: /Default for new runs/ });
  await waitFor(() => expect(select).toBeEnabled());
  await userEvent.selectOptions(select, 'local/big');
  expect(await screen.findByRole('alert')).toHaveTextContent('model local/big is not available');
  expect(select).toHaveValue('');
});

test('on a phone the picker is an icon whose name gives the pick, a dot marks the session\'s own pick, and picking still works', async () => {
  stubViewport(false);
  routes['GET /sessions/ui%3Au1'] = { key: 'ui:u1', resolvedModel: 'local/flash' };
  routes['PATCH /sessions/ui%3Au1'] = { key: 'ui:u1', model: 'local/big', resolvedModel: 'local/big' };
  openSession();
  const picker = await screen.findByRole('combobox', { name: 'Model: Default (local/flash)' });
  expect(picker).toHaveAttribute('title', 'Model for the next run: local/flash, 256k context (default)');
  expect(screen.queryByTestId('model-picked')).toBeNull();
  await userEvent.selectOptions(picker, 'local/big');
  await screen.findByRole('combobox', { name: 'Model: local/big' });
  expect(screen.getByTestId('model-picked')).toBeInTheDocument();
  expect(patches('sessions/ui%3Au1')).toEqual([{ model: 'local/big' }]);
  expect(fireEvent.keyDown(picker, { key: 'Enter' })).toBe(false);
});
