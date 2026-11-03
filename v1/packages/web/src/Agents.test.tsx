import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, expect, test, vi } from 'vitest';
import { App } from './App.tsx';
import { lineDiff } from './lineDiff.ts';

const detail = (version: number, over: object = {}) => ({
  name: 'reviewer', version, status: 'draft',
  definition: { name: 'reviewer', version, model: 'local/m', description: 'Reviews PRs.', extensions: { coding: {}, github: { maxRevisions: 2 } } },
  prompt: version === 2 ? '# Reviewer\n\nName the file.\nRun tests.' : '# Reviewer\n\nName the file.',
  rubric: { criteria: [{ id: 'catches_real_issues', checks: [{ label: 'Flags a real defect', weight: 'gate' }] }] },
  versions: [{ version: 2, status: 'draft' }, { version: 1, status: 'promoted' }],
  ...over,
});

let promoted: unknown[];
const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
  if (url === 'agents') return json({ agents: [{ name: 'reviewer', version: 2, status: 'draft', model: 'local/m', description: 'Reviews PRs.' }, { name: 'quack', version: 1, status: 'promoted' }] });
  if (url === 'agents/reviewer/promote') return promoted.push(JSON.parse(String(init?.body))), json({});
  const v = /^agents\/reviewer(?:\?version=(\d+))?$/.exec(url);
  if (v) return json(detail(Number(v[1] ?? 2)));
  return json({ sessions: [] });
});

beforeEach(() => {
  promoted = [];
  localStorage.clear();
  vi.stubGlobal('fetch', fetchMock);
  vi.stubGlobal('matchMedia', () => ({ matches: false, addEventListener() {}, removeEventListener() {} }));
});
afterEach(() => vi.unstubAllGlobals());

const main = () => within(screen.getByRole('main'));
const open = (hash: string) => {
  location.hash = hash;
  render(<App />);
};

test('the list shows each agent with its status, model, version and description', async () => {
  open('#/agents');
  const link = await main().findByRole('link', { name: /reviewer/ });
  expect(link).toHaveTextContent('Draft');
  expect(link).toHaveTextContent('local/m · Version 2');
  expect(link).toHaveTextContent('Reviews PRs.');
  expect(link).toHaveAttribute('href', '#/agents/reviewer');
  expect(main().getByRole('link', { name: /quack/ })).toHaveTextContent('Promoted');
});

test('the detail renders the prompt as markdown, the rubric as a list and settings as labelled fields', async () => {
  open('#/agents/reviewer');
  await userEvent.click(await main().findByText('Prompt'));
  expect(main().getByRole('heading', { name: 'Reviewer', level: 1 })).toBeInTheDocument();
  expect(main().getByText('Flags a real defect (gate)')).toBeInTheDocument();
  expect(main().getByText('Max revisions')).toBeInTheDocument();
  expect(main().queryByText(/maxRevisions/)).toBeNull();
  expect(main().getByRole('button', { name: 'Version 1 · promoted' })).toBeInTheDocument();
});

test('the diff reads the prompt change against the version picked', async () => {
  open('#/agents/reviewer');
  await userEvent.click(await main().findByText('Prompt changes'));
  expect(await main().findByText('Run tests.')).toBeInTheDocument();
  expect(main().getByText('Added:', { exact: false })).toBeInTheDocument();
  await userEvent.click(main().getByText('Rubric changes'));
  expect(main().getByText('No changes.')).toBeInTheDocument();
});

test('promoting asks first, then posts the shown version', async () => {
  open('#/agents/reviewer');
  await userEvent.click(await main().findByRole('button', { name: 'Promote' }));
  const dialog = screen.getByRole('dialog', { name: 'Promote reviewer?' });
  await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
  expect(promoted).toEqual([]);
  await userEvent.click(main().getByRole('button', { name: 'Promote' }));
  await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Promote' }));
  await vi.waitFor(() => expect(promoted).toEqual([{ version: 2 }]));
});

test('Agents is a bottom-bar choice, not a default', async () => {
  open('#/settings');
  expect(await screen.findByRole('checkbox', { name: 'Agents' })).not.toBeChecked();
  expect(within(screen.getAllByRole('navigation', { name: 'Main' })[1]!).queryByRole('link', { name: 'Agents' })).toBeNull();
});

test('lineDiff marks changes, trims distant context and reports no change as empty', () => {
  expect(lineDiff('a\nb', 'a\nb')).toEqual([]);
  const long = Array.from({ length: 12 }, (_, i) => `l${i}`).join('\n');
  const lines = lineDiff(long, long.replace('l6', 'x'));
  expect(lines.map((l) => l.kind)).toEqual(['gap', 'same', 'same', 'del', 'add', 'same', 'same', 'gap']);
});
