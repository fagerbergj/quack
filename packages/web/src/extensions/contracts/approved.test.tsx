import { render, screen } from '@testing-library/react';
import { expect, test } from 'vitest';
import type { SealedArtifact } from '../../api.ts';
import { ArtifactView } from '../../ArtifactPane.tsx';
import { byVersion, routes, useHarness } from '../../test-harness.ts';
import v2 from './fixtures/sleeper-schedule-v2.json';

useHarness(() => ({}));

const spec = { session: 'contracts:s1', kind: 'contract.spec', version: 20, hash: 'spechash', content: JSON.stringify(v2), sealedAt: '2026-01-01T00:00:00Z' };
const approval: SealedArtifact = {
  kind: 'contract.approved', version: 5, hash: 'rec', sealedAt: 2, summary: 'Approved version 2',
  content: JSON.stringify({ version: 2, artifact: 20, hash: 'spechash', note: 'Approved version 2', approval: '1021' }),
};

test('opening an approval shows the approved spec, not the record, and names its version rather than the record number', async () => {
  routes['GET /sessions/contracts%3As1/artifacts'] = byVersion([spec]);
  render(<ArtifactView sessionKey="contracts:s1" versions={[approval]} a={approval} onPick={() => {}} />);
  expect(await screen.findByText('Approved')).toBeInTheDocument();
  expect(screen.getAllByText(new RegExp(v2.title)).length).toBeGreaterThan(0);
  expect(screen.queryByText('approval')).toBeNull();
  expect(screen.getByRole('button', { name: /Version/ })).toHaveTextContent('v2');
});
