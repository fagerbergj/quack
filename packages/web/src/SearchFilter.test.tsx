import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { expect, test } from 'vitest';
import { artifactQuery } from './ArtifactList.tsx';
import { SearchFilter, type FilterField, type FilterValues } from './SearchFilter.tsx';
import { sessionQuery } from './sessions.ts';

const FIELDS: FilterField[] = [
  { id: 'agent', label: 'Agent', any: 'All agents', options: [{ value: 'reviewer', label: 'reviewer' }, { value: 'quack', label: 'quack' }] },
  { id: 'view', label: 'Show', any: 'Active', options: [{ value: 'archived', label: 'Archived' }] },
];

function Harness({ inMain = true }: { inMain?: boolean }) {
  const [text, setText] = useState('');
  const [values, setValues] = useState<FilterValues>({});
  const control = <SearchFilter label="Search things" text={text} onText={setText} fields={FIELDS} values={values} onValues={setValues} />;
  return inMain ? <main>{control}<output>{JSON.stringify(values)}</output><textarea aria-label="Note" /></main> : <aside>{control}</aside>;
}

test('the panel shows the fields the page declares, and each set field becomes a chip that clears it', async () => {
  render(<Harness />);
  const filter = screen.getByRole('button', { name: 'Filter' });
  expect(filter).toHaveAttribute('title', 'Filter');
  expect(screen.queryByRole('list', { name: 'Set filters' })).toBeNull();
  await userEvent.click(filter);
  const panel = screen.getByRole('dialog', { name: 'Filters' });
  expect(within(panel).getAllByRole('combobox').map((c) => c.getAttribute('id')?.split('-').at(-1))).toEqual(['agent', 'view']);
  expect(within(panel).getByRole('combobox', { name: 'Agent' })).toHaveFocus();
  await userEvent.selectOptions(within(panel).getByRole('combobox', { name: 'Agent' }), 'quack');
  await userEvent.selectOptions(within(panel).getByRole('combobox', { name: 'Show' }), 'Archived');
  expect(screen.getByRole('button', { name: 'Filter, 2 set' })).toBeInTheDocument();
  const chips = screen.getByRole('list', { name: 'Set filters' });
  expect(within(chips).getAllByRole('button').map((b) => b.getAttribute('aria-label'))).toEqual(['Remove Agent: quack', 'Remove Show: Archived']);
  await userEvent.click(within(chips).getByRole('button', { name: 'Remove Agent: quack' }));
  expect(screen.getByText('{"agent":"","view":"archived"}')).toBeInTheDocument();
  await userEvent.click(screen.getByRole('button', { name: 'Remove Show: Archived' }));
  expect(screen.queryByRole('list', { name: 'Set filters' })).toBeNull();
  expect(screen.getByRole('button', { name: 'Filter' })).toBeInTheDocument();
});

test('Escape closes the panel and gives focus back to the filter button', async () => {
  render(<Harness />);
  await userEvent.click(screen.getByRole('button', { name: 'Filter' }));
  expect(screen.getByRole('dialog', { name: 'Filters' })).toBeInTheDocument();
  await userEvent.keyboard('{Escape}');
  expect(screen.queryByRole('dialog')).toBeNull();
  expect(screen.getByRole('button', { name: 'Filter' })).toHaveFocus();
});

test('/ focuses the page search, but not while typing elsewhere or from a list outside main', async () => {
  render(<Harness />);
  await userEvent.keyboard('/');
  expect(screen.getByRole('searchbox', { name: 'Search things' })).toHaveFocus();
  expect(screen.getByRole('searchbox')).toHaveValue('');
  await userEvent.click(screen.getByRole('textbox', { name: 'Note' }));
  await userEvent.keyboard('/');
  expect(screen.getByRole('textbox', { name: 'Note' })).toHaveValue('/');
  cleanup();
  render(<Harness inMain={false} />);
  await userEvent.keyboard('/');
  expect(screen.getByRole('searchbox')).not.toHaveFocus();
});

test('Sessions build their q from the search and every filter', () => {
  expect(sessionQuery('', {})).toBe('NOT state:archived');
  expect(sessionQuery(' fix  #1927 ', { agent: 'reviewer', start: 'github', view: 'archived' })).toBe('state:archived AND agent:reviewer AND key:github\\:* AND fix* AND \\#1927*');
  expect(sessionQuery('# -', {}), 'a word with no letter or digit adds no term').toBe('NOT state:archived');
});

test('Artifacts build their q from kind, sealed and a search over the summary or session', () => {
  const now = Date.parse('2026-11-09T12:34:56.000Z');
  expect(artifactQuery('', {}, now)).toBe('');
  expect(artifactQuery('cold', { kind: 'github.review', sealed: 'day' }, now)).toBe('kind:github\\.review AND sealedAt:>=2026\\-11\\-08T12\\:00\\:00Z AND (cold* OR session:*cold*)');
});
