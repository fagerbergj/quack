import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { useState } from 'react';
import { expect, test } from 'vitest';
import type { SealedArtifact } from './api.ts';
import { ArtifactView, paneVersions } from './ArtifactPane.tsx';
import { byVersion, Fail, fetchMock, routes, useHarness } from './test-harness.ts';
import { artifactCard, artifactName } from './Artifacts.tsx';
import { loadContent } from './useContent.tsx';

useHarness(() => ({}));

const LIST = 'GET /sessions/gh%3A1/artifacts';
const review = (name: string) => readFileSync(join(process.cwd(), 'src/extensions/fixtures', `${name}.md`), 'utf8');
const rows = [
  { session: 'gh:1', kind: 'github.review', version: 1, hash: 'h1', content: review('review-changes'), sealedAt: '2026-01-01T00:00:00Z' },
  { session: 'gh:1', kind: 'github.review', version: 2, hash: 'h2', content: review('review-approve'), sealedAt: '2026-01-02T00:00:00Z' },
];
const verdict = { passed: true, gatesFailed: [], score: 1, checks: [] };
// The socket's frame: the newest review carries content, the older one only its row and outcome.
const items: SealedArtifact[] = [
  { kind: 'github.review', version: 1, hash: 'h1', sealedAt: 1, verdict, outcome: 'request_changes' },
  { kind: 'github.review', version: 2, hash: 'h2', content: rows[1]!.content, sealedAt: 2, verdict, outcome: 'approve' },
];
const reads = () => fetchMock.mock.calls.map(([u]) => decodeURIComponent(String(u))).filter((u) => u.startsWith('sessions/gh:1/artifacts'));

function Pane() {
  const [focus, setFocus] = useState<SealedArtifact>();
  const shown = paneVersions(items, focus)!;
  return <ArtifactView sessionKey="gh:1" versions={shown.versions} a={shown.a} onPick={setFocus} />;
}

test('the loader keeps a version by its hash and shares one request between concurrent readers', async () => {
  routes[LIST] = byVersion(rows);
  const [a, b] = await Promise.all([loadContent('gh:1', items[0]!), loadContent('gh:1', items[0]!)]);
  expect(a).toBe(rows[0]!.content);
  expect(b).toBe(a);
  expect(reads()).toHaveLength(1);
  await loadContent('gh:1', items[0]!);
  expect(reads(), 'the second read comes from the hash').toHaveLength(1);
  expect(reads()[0]).toMatch(/kind:github.*version:1/);
});

test('picking an older version in the pane loads it on demand and draws it as a card', async () => {
  routes[LIST] = byVersion(rows);
  render(<Pane />);
  expect(await screen.findByText('Approved')).toBeInTheDocument();
  expect(reads(), 'the newest version came with the frame').toEqual([]);
  await userEvent.click(screen.getByRole('button', { name: /^Version: v2/ }));
  await userEvent.click(screen.getByRole('menuitem', { name: /^v1/ }));
  const card = await screen.findByRole('region', { name: 'Review' });
  expect(within(card).getByText('Changes requested')).toBeInTheDocument();
  expect(reads()).toHaveLength(1);
});

test('an older version that cannot be loaded says so, and Retry reads it again', async () => {
  routes[LIST] = new Fail(500, { title: 'Server Error', detail: 'down' });
  render(<Pane />);
  await userEvent.click(await screen.findByRole('button', { name: /^Version: v2/ }));
  await userEvent.click(screen.getByRole('menuitem', { name: /^v1/ }));
  expect(await screen.findByRole('alert')).toHaveTextContent('version 1 could not be loaded: down');
  routes[LIST] = byVersion(rows);
  await userEvent.click(screen.getByRole('button', { name: 'Retry' }));
  await waitFor(() => expect(screen.getByRole('region', { name: 'Review' })).toHaveTextContent('Changes requested'));
  expect(screen.queryByRole('alert')).toBeNull();
});

test('a row without content never throws where a name, a card or a posted line is read', () => {
  const bare = (kind: string, outcome?: string): SealedArtifact => ({ kind, version: 4, hash: 'x', sealedAt: 1, verdict, outcome, links: [{ label: 'Posted', url: 'https://example.com' }] });
  expect(artifactName(bare('github.review', 'approve'))).toBe('Review');
  expect(artifactName(bare('github.review'))).toBe('Review');
  expect(artifactName(bare('contract.spec', 'v3'))).toBe('Spec');
  expect(artifactName(bare('sleeper.cards'))).toBe('Result');
  for (const kind of ['github.review', 'contract.spec', 'sleeper.cards']) expect(artifactCard(bare(kind))).toBeUndefined();
});
