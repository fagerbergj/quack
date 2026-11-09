import { createElement } from 'react';
import type { SealedArtifact } from '../../api.ts';
import { BLUE, plain, type Extension } from '../../extension.ts';
import { Contracts } from './Contracts.tsx';
import { parseSpec } from './spec.ts';
import { ContractStudio, VIEWS } from './Studio.tsx';

const KIND = 'contract.spec';
// Braces around a ring: a contract is a declared shape; full 24-unit width like the other nav icons.
const ICON = 'M8 4C6.3 4 6 5.2 6 6.5V10c0 1.1-.9 2-2 2 1.1 0 2 .9 2 2v3.5C6 18.8 6.3 20 8 20M16 4c1.7 0 2 1.2 2 2.5V10c0 1.1.9 2 2 2-1.1 0-2 .9-2 2v3.5c0 1.3-.3 2.5-2 2.5M14.5 12a2.5 2.5 0 1 1-5 0 2.5 2.5 0 1 1 5 0';

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
