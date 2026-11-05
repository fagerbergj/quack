import { obj, oneOf, opt, str } from '@earendil-works/pi-schema';
import { defineTool } from '@earendil-works/pi-durable';
import { proposeContractTool } from './approval.ts';
import { askQuestionTool } from './ask.ts';
import { kinds } from './kinds.ts';

const kind = oneOf(Object.keys(kinds), { default: 'rest', description: 'The contract kind' });

const extractTool = defineTool({
  name: 'extract_contract',
  description: 'Read an existing contract file (an OpenAPI document for rest, a .schema.json for json-schema; JSON or YAML; a .d.ts file for typescript, returned as text) and return its document, to base a spec on.',
  parameters: obj({ path: str({ minLength: 1 }), kind: opt(kind) }, { open: true }),
  replay: 'safe',
  async execute({ path, kind: k }, api, ctx) {
    const read = await api.env?.readTextFile(path, ctx);
    if (!read?.ok) return { output: [{ type: 'text', text: `${path}: cannot be read` }], isError: true };
    const doc = kinds[k ?? 'rest'].extract(read.value);
    return { output: [{ type: 'text', text: typeof doc === 'string' ? doc : JSON.stringify(doc, null, 2) }] };
  },
});

const guideTool = defineTool({
  name: 'read_guide',
  description: 'The authoring guide for a contract kind: its rules, and the choices to ask the person. Cite its headings as guide refs.',
  parameters: obj({ kind: opt(kind) }, { open: true }),
  replay: 'safe',
  async execute({ kind: k }) {
    return { output: [{ type: 'text', text: kinds[k ?? 'rest'].skill }] };
  },
});

export const contractTools = [askQuestionTool, extractTool, guideTool, proposeContractTool];
