import { createElement } from 'react';
import type { SealedArtifact } from '../../api.ts';
import { BLUE, plain, type Extension } from '../../extension.ts';
import { Contracts } from './Contracts.tsx';
import { parseSpec } from './spec.ts';
import { ContractStudio, VIEWS } from './Studio.tsx';

const KIND = 'contract.spec';
// Open braces, a contract's declared shape; spaced to the full 24-unit width like the other nav icons.
const ICON = 'M7.5 3.5C5.6 3.5 5 4.6 5 6.2V9.6c0 1.3-.8 2.4-2.2 2.4 1.4 0 2.2 1.1 2.2 2.4v3.4c0 1.6.6 2.7 2.5 2.7M16.5 3.5c1.9 0 2.5 1.1 2.5 2.7v3.4c0 1.3.8 2.4 2.2 2.4-1.4 0-2.2 1.1-2.2 2.4v3.4c0 1.6-.6 2.7-2.5 2.7';

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
    label: 'contract', noun: () => 'Contract', match: (r) => r.key.startsWith('contracts:'), icon: ICON, tint: BLUE,
    name: (r) => plain(r.title ?? 'New contract'),
    words: () => ({ run: 'Drafting the contract', done: 'Draft ready', none: 'Starting' }),
  },
};
