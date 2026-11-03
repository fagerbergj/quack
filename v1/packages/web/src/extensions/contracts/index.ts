import { createElement } from 'react';
import type { SealedArtifact } from '../../api.ts';
import type { Extension } from '../../extension.ts';
import { RestView } from './RestView.tsx';
import { parseSpec } from './spec.ts';

const KIND = 'contract.spec';

const view = (a: SealedArtifact, full: boolean) => {
  const spec = a.kind === KIND ? parseSpec(a.content) : undefined;
  return spec && createElement(RestView, { key: a.version, spec, full });
};

export const contracts: Extension = {
  artifact: (a) => (a.kind === KIND ? { name: 'Spec' } : undefined),
  card: (a) => view(a, false),
  full: (a) => view(a, true),
  versionOf: (a) => (a.kind === KIND ? parseSpec(a.content)?.version : undefined),
};
