import { choicesAt, unplaced } from './Info.tsx';
import { download, Header, type ViewProps } from './RestView.tsx';
import { Changes, MoreAbout, Purpose, ShortChange, useFlash, useItem } from './Review.tsx';
import { SchemaTree } from './SchemaTree.tsx';
import { fields, type ContractSpec, type Field, type Json } from './spec.ts';

const count = (n: number) => `${n} ${n === 1 ? 'field' : 'fields'}`;
const file = (doc: Json) => ({ label: 'JSON Schema file', save: () => download(`${JSON.stringify(doc, null, 2)}\n`, 'application/schema+json', 'schema.json') });

// A schema with no properties (a string grammar, say) has no field tree, so it shows as written.
function Body({ spec, doc, rows }: { spec: ContractSpec; doc: Json; rows: Field[] }) {
  const item = useItem('#schema');
  const flash = useFlash<HTMLElement>('#schema');
  return (
    <section ref={flash.ref} tabIndex={-1} data-ptr="#schema" aria-label={rows.length ? 'Fields' : 'Schema'} className={`flex min-w-0 scroll-mt-16 flex-col gap-3 rounded-[18px] border border-line bg-surface p-4 outline-none ${flash.cls}`}>
      {item && <ShortChange item={item} />}
      {rows.length ? <SchemaTree rows={rows} whyAt={(p) => choicesAt(spec.choices, p)} />
        : <pre className="m-0 overflow-x-auto font-mono text-xs leading-relaxed whitespace-pre-wrap text-ink-2 [overflow-wrap:anywhere]">{JSON.stringify(doc, null, 2)}</pre>}
    </section>
  );
}

export function JsonSchemaView({ spec, verdict }: ViewProps) {
  const doc = spec.document as Json;
  const rows = fields(doc, doc, '');
  return (
    <div className="@container flex min-w-0 flex-col gap-6">
      <Header spec={spec} count={count(rows.length)} file={file(doc)} verdict={verdict} info={unplaced(spec.choices, new Set(rows.map((f) => f.ptr)))} />
      <Purpose spec={spec} />
      <Changes spec={spec} />
      <Body spec={spec} doc={doc} rows={rows} />
      <MoreAbout spec={spec} />
    </div>
  );
}
