import { createElement } from 'react';
import type { SealedArtifact } from '../../api.ts';
import { BLUE, plain, type Extension } from '../../extension.ts';
import { Contracts } from './Contracts.tsx';
import { RestView } from './RestView.tsx';
import { parseSpec } from './spec.ts';
import { contractOf } from './status.ts';

const KIND = 'contract.spec';
const ICON = 'M7 3h7l4 4v14H7zM14 3v4h4M9.5 14l2 2 3.5-4';

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
  nav: { route: 'contracts', label: 'Contracts', icon: ICON, page: Contracts },
  // A contract session is named by its latest spec, and reads its status the way the Contracts page does.
  session: {
    label: 'contract', noun: () => 'Contract', group: 'Contracts', match: (r) => r.key.startsWith('contracts:'), icon: ICON, tint: BLUE,
    name: (r) => plain(contractOf(r).name),
    words: () => ({ kind: KIND, w: { run: 'Drafting the contract', pass: 'Spec ready', fail: "Didn't pass its checks", none: 'Starting', sealed: 'Spec ready' } }),
    delivered: (r) => { const c = contractOf(r); return c.status.startsWith('Approved') ? { line: c.status, tone: 'ok' } : undefined; },
  },
};
