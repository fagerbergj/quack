import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, expect, test, vi } from 'vitest';
import { More } from '../../More.tsx';
import { Remarkable } from './Remarkable.tsx';

const nb = (id: string, name: string, state: string, over: object = {}) => ({ id, name, folder: false, modified: '2026-10-01T12:00:00Z', state, ...over });
const TREE = [
  nb('a', 'Campaign Notes', 'changed'),
  nb('b', 'Fresh pad', 'new'),
  nb('c', 'Broken pad', 'failed', { error: 'distributor answered 500' }),
  nb('d', 'Old pad', 'current'),
  { id: 'f', name: 'Personal', folder: true, modified: '', children: [nb('e', 'Dice log', 'changed'), { id: 'g', name: 'DND', folder: true, modified: '', children: [nb('h', 'Maps', 'new')] }] },
];

const calls: Array<[string, RequestInit | undefined]> = [];
beforeEach(() => {
  calls.length = 0;
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    calls.push([url, init]);
    return new Response(JSON.stringify(init?.method === 'POST' ? { batch: 'x' } : { tree: TREE }), { status: 200 });
  }));
});
afterEach(() => vi.unstubAllGlobals());

test('each notebook shows its ingest state, and only changed ones start selected', async () => {
  render(<Remarkable rows={undefined} />);
  expect(await screen.findByText('2 changed since last ingest')).toBeInTheDocument();
  for (const [name, label] of [['Campaign Notes', 'Changed'], ['Fresh pad', 'New'], ['Broken pad', 'Ingest failed'], ['Old pad', 'Up to date']]) {
    expect(within(screen.getByLabelText(`Select ${name}`).closest('label')!).getByText(label)).toBeInTheDocument();
  }
  expect(screen.getByLabelText('Select Campaign Notes')).toBeChecked();
  expect(screen.getByLabelText('Select Fresh pad')).not.toBeChecked();
  expect(screen.getByText('2 selected')).toBeInTheDocument();
});

test('a failed notebook keeps its error behind a disclosure', async () => {
  render(<Remarkable rows={undefined} />);
  await screen.findByText('Broken pad');
  const details = screen.getByText('Details').closest('details')!;
  expect(details).not.toHaveAttribute('open');
  expect(within(details).getByText('distributor answered 500')).toBeInTheDocument();
});

test('folders navigate by breadcrumb chips and selection survives the move', async () => {
  render(<Remarkable rows={undefined} />);
  await userEvent.click(await screen.findByRole('button', { name: /Personal/ }));
  const crumbs = screen.getByRole('navigation', { name: 'Folder' });
  expect(within(crumbs).getByText('Personal')).toHaveAttribute('aria-current', 'page');
  expect(screen.getByLabelText('Select Dice log')).toBeChecked();
  await userEvent.click(screen.getByRole('button', { name: /DND/ }));
  await userEvent.click(screen.getByLabelText('Select Maps'));
  await userEvent.click(within(screen.getByRole('navigation', { name: 'Folder' })).getByRole('button', { name: 'My files' }));
  expect(screen.getByText('3 selected')).toBeInTheDocument();
});

test('Ingest sends the selected ids, then clears the selection', async () => {
  render(<Remarkable rows={undefined} />);
  await userEvent.click(await screen.findByLabelText('Select Broken pad'));
  await userEvent.click(screen.getByRole('button', { name: 'Ingest 3 notebooks' }));
  const post = calls.find(([, i]) => i?.method === 'POST')!;
  expect(post[0]).toBe('apps/remarkable/ingest');
  expect(JSON.parse(post[1]!.body as string).ids.sort()).toEqual(['a', 'c', 'e']);
  expect(await screen.findByText('0 selected')).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Ingest' })).toBeDisabled();
});

test('More lists every app as a tile and the reMarkable tile says how many changed', async () => {
  render(<More />);
  for (const name of ['Sleeper', 'reMarkable']) expect(screen.getByRole('link', { name: new RegExp(name) })).toBeInTheDocument();
  expect(await within(screen.getByRole('link', { name: /reMarkable/ })).findByText('2 changed')).toBeInTheDocument();
});
