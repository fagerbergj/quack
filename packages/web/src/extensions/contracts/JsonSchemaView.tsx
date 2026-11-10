import { choicesAt, unplaced } from './Info.tsx';
import { Changes, Compatibility, download, Header, Purpose, type ViewProps } from './RestView.tsx';
import { SchemaTree } from './SchemaTree.tsx';
import { fields, type Json } from './spec.ts';

const count = (n: number) => `${n} ${n === 1 ? 'field' : 'fields'}`;
const file = (doc: Json) => ({ label: 'JSON Schema file', save: () => download(`${JSON.stringify(doc, null, 2)}\n`, 'application/schema+json', 'schema.json') });

export function JsonSchemaView({ spec, verdict }: ViewProps) {
  const doc = spec.document as Json;
  const rows = fields(doc, doc, '');
  return (
    <div className="@container flex min-w-0 flex-col gap-4">
      <Header spec={spec} count={count(rows.length)} file={file(doc)} verdict={verdict} info={unplaced(spec.choices, new Set(rows.map((f) => f.ptr)))} />
      <Changes spec={spec} />
      <Compatibility spec={spec} />
      <Purpose spec={spec} />
      <section aria-label="Fields" className="min-w-0 rounded-[18px] border border-line bg-surface p-4"><SchemaTree rows={rows} whyAt={(p) => choicesAt(spec.choices, p)} /></section>
    </div>
  );
}
