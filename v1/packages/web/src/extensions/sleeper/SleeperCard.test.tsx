import { render, screen, within } from '@testing-library/react';
import { expect, test } from 'vitest';
import { ArtifactBoundary } from '../../ArtifactBoundary.tsx';
import { artifactCard } from '../../Artifacts.tsx';
import { Work } from '../../Work.tsx';
import type { SealedArtifact } from '../../api.ts';
import digest7 from './fixtures/7-digest.json';
import retro7 from './fixtures/7-retro.json';
import history8 from './fixtures/8-history.json';
import lineup8 from './fixtures/8-lineup.json';
import finder8 from './fixtures/8-trade-finder.json';
import trends8 from './fixtures/8-trends.json';
import waivers8 from './fixtures/8-waivers.json';

const sealed = (content: string, kind = 'sleeper.cards'): SealedArtifact => ({ kind, version: 1, hash: 'h', content, sealedAt: 1, verdict: null });
const drawer = (a: SealedArtifact) => render(<Work artifact={a} onClose={() => {}} />);

test('every card kind draws as a card, never the JSON tree', () => {
  for (const c of [lineup8, waivers8, finder8, trends8, digest7, retro7, history8]) {
    const { unmount } = render(<>{artifactCard(c as unknown as SealedArtifact)}</>);
    expect(screen.getAllByRole('article')).toHaveLength(1);
    expect(screen.queryByLabelText('JSON')).not.toBeInTheDocument();
    unmount();
  }
});

test('waivers show ranked candidates, who to drop, the why and news links', () => {
  drawer(waivers8 as unknown as SealedArtifact);
  const w = within(screen.getByRole('article', { name: 'Waivers' }));
  expect(w.getByText('+ Pat Freiermuth')).toBeInTheDocument();
  expect(w.getAllByText(/− Jordyn Tyson/).length).toBeGreaterThan(0);
  expect(w.getAllByText(/News \(\d+\)/).length).toBeGreaterThan(0);
  expect(w.getAllByRole('link').length).toBeGreaterThan(0);
});

test('a card whose shape does not match falls back to the JSON tree, not text', () => {
  const bad = sealed(JSON.stringify({ kind: 'waivers', candidates: [{ rank: 1 }] }));
  render(<ArtifactBoundary artifact={bad}>{artifactCard(bad)}</ArtifactBoundary>);
  expect(screen.getByLabelText('JSON')).toBeInTheDocument();
});

test('an unknown card kind opens as the JSON tree', () => {
  drawer(sealed(JSON.stringify({ kind: 'novel', rows: [1, 2] })));
  expect(screen.getByLabelText('JSON')).toBeInTheDocument();
  expect(screen.getByText('2 items')).toBeInTheDocument();
});

test('a non-sleeper JSON artifact shows the tree instead of a text wall', () => {
  drawer(sealed(JSON.stringify({ a: { b: 1 } }), 'other.json'));
  expect(screen.getByLabelText('JSON')).toBeInTheDocument();
});
