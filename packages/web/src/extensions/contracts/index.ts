import { createElement } from 'react';
import type { SealedArtifact } from '../../api.ts';
import { BLUE, plain, type Extension } from '../../extension.ts';
import { Contracts } from './Contracts.tsx';
import { parseSpec } from './spec.ts';
import { ContractStudio, VIEWS } from './Studio.tsx';

const KIND = 'contract.spec';
const ICON = 'M7 3h7l4 4v14H7zM14 3v4h4M9.5 14l2 2 3.5-4';

const NAMES: Record<string, string> = { 'contract.approved': 'Approval', 'contract.changes': 'Changes requested' };

const view = (a: SealedArtifact, full: boolean) => {
  const spec = a.kind === KIND ? parseSpec(a.content) : undefined;
  return spec && createElement(VIEWS[spec.kind], { key: a.version, spec, full, verdict: a.verdict });
};

export const contracts: Extension = {
  artifact: (a) => (a.kind === KIND ? { name: 'Spec', titled: true } : NAMES[a.kind] ? { name: NAMES[a.kind]! } : undefined),
  card: (a) => view(a, false),
  full: (a) => view(a, true),
  versionOf: (a) => (a.kind === KIND ? parseSpec(a.content)?.version : undefined),
  // The contract is drawn beside its conversation, and approved there.
  studio: { match: (key) => key.startsWith('contracts:'), label: 'Contract', owns: (a) => a.tool === 'propose_contract', Pane: ContractStudio },
  nav: { route: 'contracts', label: 'Contracts', icon: ICON, page: Contracts },
  session: {
    label: 'contract', noun: () => 'Contract', group: 'Contracts', match: (r) => r.key.startsWith('contracts:'), icon: ICON, tint: BLUE,
    name: (r) => plain(r.title ?? 'New contract'),
    words: () => ({ run: 'Drafting the contract', done: 'Draft ready', none: 'Starting' }),
  },
};
