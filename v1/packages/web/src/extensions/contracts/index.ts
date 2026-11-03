import { createElement } from 'react';
import type { SealedArtifact } from '../../api.ts';
import type { Extension } from '../../extension.ts';
import { Contracts } from './Contracts.tsx';
import { RestView } from './RestView.tsx';
import { parseSpec } from './spec.ts';

const KIND = 'contract.spec';

const NAMES: Record<string, string> = { 'contract.approved': 'Approval', 'contract.changes': 'Changes requested' };

const view = (a: SealedArtifact, full: boolean) => {
  const spec = a.kind === KIND ? parseSpec(a.content) : undefined;
  return spec && createElement(RestView, { key: a.version, spec, full });
};

export const contracts: Extension = {
  artifact: (a) => (a.kind === KIND ? { name: 'Spec' } : NAMES[a.kind] ? { name: NAMES[a.kind]! } : undefined),
  card: (a) => view(a, false),
  full: (a) => view(a, true),
  versionOf: (a) => (a.kind === KIND ? parseSpec(a.content)?.version : undefined),
  nav: { route: 'contracts', label: 'Contracts', icon: 'M7 3h7l4 4v14H7zM14 3v4h4M9.5 14l2 2 3.5-4', page: Contracts },
};
