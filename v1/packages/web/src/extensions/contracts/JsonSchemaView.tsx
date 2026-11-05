import type { Verdict } from '../../api.ts';
import { choicesAt, unplaced } from './Info.tsx';
import { Changes, Compatibility, decisionCount, download, Header, Purpose, type ViewProps } from './RestView.tsx';
import { SchemaTree } from './SchemaTree.tsx';
import { fields, type ContractSpec, type Json } from './spec.ts';

const count = (n: number) => `${n} ${n === 1 ? 'field' : 'fields'}`;
const file = (doc: Json) => ({ label: 'JSON Schema file', save: () => download(`${JSON.stringify(doc, null, 2)}\n`, 'application/schema+json', 'schema.json') });

// The ledger card: the top-level fields only; nested fields, reasons and decisions live on the full page.
function CardView({ spec, doc, verdict }: { spec: ContractSpec; doc: Json; verdict?: Verdict | null }) {
  const rows = fields(doc, doc, '');
  return (
    <div className="@container flex min-w-0 flex-col gap-4">
      <Header spec={spec} count={count(rows.filter((f) => f.depth === 0).length)} full={false} file={file(doc)} verdict={verdict} />
      <Purpose spec={spec} />
      <ul aria-label="Fields" className="m-0 flex list-none flex-col gap-1.5 p-0">
        {rows.filter((f) => f.depth === 0).map((f) => <li key={f.ptr} className="flex items-center gap-2.5 text-[13px]"><code className="min-w-0 flex-1 truncate font-mono">{f.name}</code><span className="font-mono text-xs text-success">{f.type}</span>{f.required && <span className="text-[11px] tracking-wide text-danger">REQUIRED</span>}</li>)}
      </ul>
      <p className="m-0 text-[13px] text-muted">{decisionCount(spec)}</p>
    </div>
  );
}

export function JsonSchemaView({ spec, full = false, verdict }: ViewProps) {
  const doc = spec.document as Json;
  if (!full) return <CardView spec={spec} doc={doc} verdict={verdict} />;
  const rows = fields(doc, doc, '');
  return (
    <div className="@container flex min-w-0 flex-col gap-4">
      <Header spec={spec} count={count(rows.length)} full file={file(doc)} verdict={verdict} info={unplaced(spec.choices, new Set(rows.map((f) => f.ptr)))} />
      <Changes spec={spec} />
      <Compatibility spec={spec} />
      <Purpose spec={spec} />
      <section aria-label="Fields" className="min-w-0 rounded-[18px] border border-line bg-surface p-4"><SchemaTree rows={rows} whyAt={(p) => choicesAt(spec.choices, p)} /></section>
    </div>
  );
}
