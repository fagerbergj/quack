import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, expect, test, vi } from 'vitest';
import { agentGroups } from './Agents.tsx';
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

test('the list shows each agent with its description and one version whose mark names its status', async () => {
  open('#/agents');
  const link = await main().findByRole('link', { name: /reviewer/ });
  expect(link.textContent?.match(/v\d+/g)).toEqual(['v2']);
  expect(within(link).getByRole('img')).toHaveAccessibleName('Draft');
  expect(link).not.toHaveTextContent(/Draft|Promoted|Version/);
  expect(link).toHaveTextContent('Reviews PRs.');
  expect(link).toHaveAttribute('href', '#/agents/reviewer');
  expect(link).toHaveAccessibleName('reviewer Reviews PRs. local/m v2 Draft');
  expect(within(main().getByRole('link', { name: /quack/ })).getByRole('img')).toHaveAccessibleName('Promoted: runs unattended');
});

test('promoted agents lead, and a model the agents differ on shows on each row', async () => {
  open('#/agents');
  await main().findByRole('link', { name: /reviewer/ });
  expect(main().getAllByRole('link').map((l) => l.getAttribute('href'))).toEqual(['#/agents/quack', '#/agents/reviewer']);
  expect(main().getByRole('link', { name: /reviewer/ })).toHaveTextContent('local/m');
  expect(main().getByText('1 promoted')).toBeInTheDocument();
  expect(main().queryByText(/all on/)).toBeNull();
});

test('the detail renders the prompt as markdown, the rubric as a list and settings as labelled fields', async () => {
  open('#/agents/reviewer');
  await userEvent.click(await main().findByText('Prompt'));
  expect(main().getByRole('heading', { name: 'Reviewer', level: 1 })).toBeInTheDocument();
  expect(main().getByText('Flags a real defect (gate)')).toBeInTheDocument();
  expect(main().getByText('Max revisions')).toBeInTheDocument();
  expect(main().queryByText(/maxRevisions/)).toBeNull();
  expect(main().getByRole('button', { name: 'v1 Promoted: runs unattended' })).toBeInTheDocument();
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

test('agents sharing a name prefix group under it, and a model every agent shares is said once', () => {
  const agent = (name: string, status: 'draft' | 'promoted' = 'draft') => ({ name, version: 1, status, model: 'local/m' });
  const groups = agentGroups([agent('sleeper-retro'), agent('reviewer', 'promoted'), agent('sleeper-digest'), agent('remarkable-ingest'), agent('contracts')]);
  expect(groups.map((g) => [g.prefix, g.agents.map((a) => a.name)])).toEqual([
    ['', ['reviewer', 'contracts', 'remarkable-ingest']],
    ['sleeper', ['sleeper-digest', 'sleeper-retro']],
  ]);
});

test('with one model for all, the lead names it once and no row repeats it', async () => {
  fetchMock.mockImplementationOnce(async () => Response.json({ agents: [{ name: 'sleeper-retro', version: 6, status: 'draft', model: 'local/m' }, { name: 'sleeper-digest', version: 3, status: 'draft', model: 'local/m' }] }));
  open('#/agents');
  const group = await main().findByRole('region', { name: 'sleeper agents' });
  expect(within(group).getByRole('heading')).toHaveTextContent('sleeper2 agents');
  expect(main().getByText('local/m')).toBeInTheDocument();
  expect(within(group).getAllByRole('link').map((l) => l.textContent)).toEqual(['sleeper-digest v3 ›', 'sleeper-retro v6 ›']);
});

test('agents with no pinned model read as all on the default model, with no chip', async () => {
  fetchMock.mockImplementationOnce(async () => Response.json({ agents: [{ name: 'contracts', version: 19, status: 'draft' }, { name: 'reviewer', version: 16, status: 'promoted' }] }));
  open('#/agents');
  await main().findByRole('link', { name: /contracts/ });
  expect(main().getByText(/all on the default model/)).toBeInTheDocument();
  expect(main().queryByText('Default model')).toBeNull();
});
