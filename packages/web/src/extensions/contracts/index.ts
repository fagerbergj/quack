import { createElement } from 'react';
import type { SealedArtifact } from '../../api.ts';
import { BLUE, plain, type Extension } from '../../extension.ts';
import { ChangesAsked } from './Comments.tsx';
import { Contracts } from './Contracts.tsx';
import { parseSpec, specVersion } from './spec.ts';
import { specNumbers } from './status.ts';
import { ContractStudio, Standalone } from './Studio.tsx';

const KIND = 'contract.spec';
// Open braces, a contract's declared shape; spaced to the full 24-unit width like the other nav icons.
const ICON = 'M7.5 3.5C5.6 3.5 5 4.6 5 6.2V9.6c0 1.3-.8 2.4-2.2 2.4 1.4 0 2.2 1.1 2.2 2.4v3.4c0 1.6.6 2.7 2.5 2.7M16.5 3.5c1.9 0 2.5 1.1 2.5 2.7v3.4c0 1.3.8 2.4 2.2 2.4-1.4 0-2.2 1.1-2.2 2.4v3.4c0 1.6-.6 2.7-2.5 2.7';

const NAMES: Record<string, string> = { 'contract.approved': 'Approval', 'contract.changes': 'Changes requested' };

const view = (a: SealedArtifact) => {
  if (a.kind === 'contract.changes' && a.content !== undefined) return createElement(ChangesAsked, { content: a.content });
  const spec = a.kind === KIND && a.content !== undefined ? parseSpec(a.content) : undefined;
  return spec && createElement(Standalone, { key: a.version, spec, verdict: a.verdict });
};

export const contracts: Extension = {
  artifact: (a) => (a.kind === KIND ? { name: 'Spec' } : NAMES[a.kind] ? { name: NAMES[a.kind]! } : undefined),
  card: view,
  // The seal stores the spec's own version as the row's outcome ("v3", "v3 typescript"), so no content is read; a row without one reads as its sealed number.
  versionOf: (a) => (a.kind === KIND ? specVersion(a) : undefined),
  // The Sessions API rows carry no outcome, so the contracts app's own artifact-to-version list supplies it.
  label: async (key, kind, rows) => {
    const numbers = kind === KIND ? await specNumbers(key) : undefined;
    return numbers ? rows.map((a) => (numbers.has(a.version) ? { ...a, outcome: `v${numbers.get(a.version)}` } : a)) : rows;
  },
  // The contract is drawn beside its conversation, and approved there.
  studio: { match: (key) => key.startsWith('contracts:'), label: 'Contract', owns: (a) => a.tool === 'propose_contract', Pane: ContractStudio },
  nav: { route: 'contracts', label: 'Contracts', icon: ICON, page: Contracts },
  session: {
    label: 'contract', noun: () => 'Contract', match: (r) => r.key.startsWith('contracts:'), icon: ICON, tint: BLUE,
    name: (r) => plain(r.title ?? 'New contract'),
    words: () => ({ run: 'Drafting the contract', done: 'Draft ready', none: 'Starting' }),
  },
};
