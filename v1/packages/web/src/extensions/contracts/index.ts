import { createElement } from 'react';
import type { Extension } from '../../extension.ts';
import { RestView } from './RestView.tsx';
import { parseSpec } from './spec.ts';

const KIND = 'contract.spec';

export const contracts: Extension = {
  artifact: (a) => (a.kind === KIND ? { name: 'Spec' } : undefined),
  card: (a) => {
    const spec = a.kind === KIND ? parseSpec(a.content) : undefined;
    return spec && createElement(RestView, { key: a.version, spec });
  },
};
