import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { expect, test } from 'vitest';
import { App } from './App.tsx';
import { Fail, FakeSocket, fetchMock, routes, snapshot, useHarness } from './test-harness.ts';

const MODELS = { models: [{ name: 'local/flash', contextWindow: 262144 }, { name: 'local/big', contextWindow: 131072, images: true }, { name: 'local/peer/deepseek' }], defaultModel: 'local/flash' };

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

test('the composer shows the model the session runs by its short name, and a pick is sent as the session\'s model', async () => {
  routes['GET /sessions/ui%3Au1'] = { key: 'ui:u1', resolvedModel: 'local/flash' };
  routes['PATCH /sessions/ui%3Au1'] = { key: 'ui:u1', model: 'local/peer/deepseek', resolvedModel: 'local/peer/deepseek' };
  openSession();
  const picker = await screen.findByRole('combobox', { name: 'Model:' });
  await waitFor(() => expect(picker).toHaveValue('local/flash'));
  expect(within(picker).getAllByRole('option').map((o) => o.textContent)).toEqual(['flash', 'big', 'deepseek']);
  expect(picker).toHaveDisplayValue('flash');
  expect(picker).toHaveAccessibleDescription('Model for the next run: local/flash, 256k context, from Settings');
  expect(picker.parentElement).not.toBe(screen.getByRole('textbox', { name: 'Message' }).parentElement);
  expect(picker.parentElement).not.toHaveTextContent(/default/i);
  expect([picker.title, ...within(picker).getAllByRole('option').map((o) => o.title)].join()).not.toMatch(/default/i);
  await userEvent.selectOptions(picker, 'local/peer/deepseek');
  await waitFor(() => expect(picker).toHaveValue('local/peer/deepseek'));
  expect(patches('sessions/ui%3Au1')).toEqual([{ model: 'local/peer/deepseek' }]);
  expect(picker).toHaveAccessibleDescription('Model for the next run: local/peer/deepseek, unknown context, this session\'s pick');
});

test('choosing the Settings model clears the session\'s pick, so the session follows Settings again', async () => {
  routes['GET /sessions/ui%3Au1'] = { key: 'ui:u1', model: 'local/big', resolvedModel: 'local/big' };
  routes['PATCH /sessions/ui%3Au1'] = { key: 'ui:u1', resolvedModel: 'local/flash' };
  openSession();
  const picker = await screen.findByRole('combobox', { name: 'Model:' });
  await waitFor(() => expect(picker).toHaveAccessibleDescription('Model for the next run: local/big, 128k context, this session\'s pick'));
  expect(within(picker).getByRole('option', { name: 'flash' })).toHaveAttribute('title', 'Settings model; choosing it follows Settings again');
  expect(within(picker).getByRole('option', { name: 'big' })).not.toHaveAttribute('title');
  await userEvent.selectOptions(picker, 'local/flash');
  await waitFor(() => expect(picker).toHaveAccessibleDescription('Model for the next run: local/flash, 256k context, from Settings'));
  expect(patches('sessions/ui%3Au1')).toEqual([{ model: null }]);
});

test('a resolved model the list no longer offers is still the one shown, first in the list', async () => {
  routes['GET /sessions/ui%3Au1'] = { key: 'ui:u1', resolvedModel: 'local/gone' };
  openSession();
  const picker = await screen.findByRole('combobox', { name: 'Model:' });
  await waitFor(() => expect(picker).toHaveValue('local/gone'));
  expect(within(picker).getAllByRole('option').map((o) => o.textContent)).toEqual(['gone', 'flash', 'big', 'deepseek']);
  expect(picker).toHaveAccessibleDescription('Model for the next run: local/gone, unknown context, from its agent');
});

test('the composer shows the session\'s own pick, and a refused pick keeps it and says why', async () => {
  routes['GET /sessions/ui%3Au1'] = { key: 'ui:u1', model: 'local/big', resolvedModel: 'local/big' };
  routes['PATCH /sessions/ui%3Au1'] = new Fail(400, { detail: 'model local/peer/deepseek is not available; available: local/big' });
  openSession();
  const picker = await screen.findByRole('combobox', { name: 'Model:' });
  await waitFor(() => expect(picker).toHaveValue('local/big'));
  await userEvent.selectOptions(picker, 'local/peer/deepseek');
  await waitFor(() => expect(picker).toHaveAccessibleDescription('model local/peer/deepseek is not available; available: local/big'));
  expect(picker).toHaveValue('local/big');
  expect(picker).toHaveAttribute('aria-invalid', 'true');
});

test('a session without a pick whose agent names a model is described as running the agent\'s model', async () => {
  routes['GET /sessions/ui%3Au1'] = { key: 'ui:u1', resolvedModel: 'local/big' };
  openSession();
  const picker = await screen.findByRole('combobox', { name: 'Model:' });
  await waitFor(() => expect(picker).toHaveAccessibleDescription('Model for the next run: local/big, 128k context, from its agent'));
  expect(picker).toHaveDisplayValue('big');
});

test('Enter on the focused picker never sends the draft', async () => {
  routes['GET /sessions/ui%3Au1'] = { key: 'ui:u1', resolvedModel: 'local/flash' };
  const ws = openSession();
  await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'half a thought');
  const picker = await screen.findByRole('combobox', { name: 'Model:' });
  picker.focus();
  // A browser's implicit submission is Enter's default action on a form control; false means the picker cancelled it.
  expect(fireEvent.keyDown(picker, { key: 'Enter' })).toBe(false);
  await userEvent.keyboard('{Enter}');
  expect(ws.sent).toEqual([]);
  expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue('half a thought');
});

test('two providers offering one id show their provider, so no two options read alike', async () => {
  routes['GET /models'] = { models: [{ name: 'local/m', contextWindow: 4096 }, { name: 'hosted/m', contextWindow: 4096 }, { name: 'local/solo', contextWindow: 4096 }] };
  routes['GET /sessions/ui%3Au1'] = { key: 'ui:u1', resolvedModel: 'local/m' };
  openSession();
  const picker = await screen.findByRole('combobox', { name: 'Model:' });
  expect(within(picker).getAllByRole('option').map((o) => o.textContent)).toEqual(['local/m', 'hosted/m', 'solo']);
});

test('with no models listed the composer shows no picker', async () => {
  routes['GET /models'] = { models: [] };
  openSession();
  await screen.findByRole('textbox', { name: 'Message' });
  expect(screen.queryByRole('combobox', { name: 'Model:' })).toBeNull();
});

test('the Settings page shows the default model by name and sets another on the server', async () => {
  routes['PATCH /settings'] = { defaultModel: 'local/big' };
  location.hash = '#/settings';
  render(<App />);
  const select = await screen.findByRole('combobox', { name: /Default for new runs/ });
  await waitFor(() => expect(select).toHaveValue('local/flash'));
  expect(within(select).getAllByRole('option').map((o) => o.textContent)).toEqual(['flash', 'big', 'deepseek']);
  expect(select).toHaveAccessibleDescription('local/flash, 256k context');
  expect(screen.getByRole('main')).not.toHaveTextContent(/server default/i);
  routes['GET /models'] = { ...MODELS, defaultModel: 'local/big' };
  await userEvent.selectOptions(select, 'local/big');
  await waitFor(() => expect(select).toHaveValue('local/big'));
  expect(patches('settings')).toEqual([{ defaultModel: 'local/big' }]);
});

test('the Settings page names a refused default', async () => {
  routes['PATCH /settings'] = new Fail(400, { detail: 'model local/big is not available; available: local/flash' });
  location.hash = '#/settings';
  render(<App />);
  const select = await screen.findByRole('combobox', { name: /Default for new runs/ });
  await waitFor(() => expect(select).toBeEnabled());
  await userEvent.selectOptions(select, 'local/big');
  expect(await screen.findByRole('alert')).toHaveTextContent('model local/big is not available');
  expect(select).toHaveValue('local/flash');
});
