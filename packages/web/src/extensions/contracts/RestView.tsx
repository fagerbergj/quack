import { useContext, useState, type ReactNode } from 'react';
import { SessionKeyContext } from '../../extension.ts';
import { Markdown } from '../../Markdown.tsx';
import { StatusIcon } from '../../StatusIcon.tsx';
import type { Verdict } from '../../api.ts';
import { DiffContext, opUnits, useAnyMark, useMark } from './diff.ts';
import { choicesAt, Info, Inline, unplaced } from './Info.tsx';
import { MARKED, MarkTag, SchemaTree } from './SchemaTree.tsx';
import { fields, operations, paramFields, resolve, restPointers, toYaml, type Choice, type ContractSpec, type Json, type Operation } from './spec.ts';

const METHOD: Record<string, string> = { GET: 'bg-link-bg text-link', POST: 'bg-success-bg text-success', DELETE: 'bg-danger-bg text-danger' };
export const label = 'text-xs font-semibold uppercase tracking-wider text-muted';
const dt = 'w-20 flex-none whitespace-nowrap';
export const card = 'rounded-[18px] border border-line bg-surface';
// The pane header's actions: one 44px icon button each, named by aria-label and title.
const round = 'grid h-11 w-11 flex-none cursor-pointer place-items-center rounded-full border border-line-strong bg-transparent text-ink-2 no-underline hover:text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-link';
const svg = (d: string) => <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={d} /></svg>;

function Method({ m }: { m: string }) {
  return <span className={`flex-none rounded-md px-2 py-0.5 font-mono text-xs font-semibold ${METHOD[m] ?? 'bg-warning-bg text-warning'}`}>{m}</span>;
}

// Under a comparison the Studio lists the real changes, so the agent's account of them stands back.
export function Changes({ spec }: { spec: ContractSpec }) {
  const byId = new Map(spec.choices.map((c) => [c.id, c]));
  if (useContext(DiffContext) || !spec.changes?.length) return null;
  return (
    <section aria-label="What changed" className={`${card} overflow-hidden`}>
      <h3 className={`m-0 px-4 py-3 font-sans ${label}`}>Changed from version {spec.version - 1}</h3>
      <ul className="m-0 list-none p-0">
        {spec.changes.map((c) => {
          const choice = byId.get(c.choice);
          return (
            <li key={c.choice} className="flex items-start gap-3 border-t border-line px-4 py-3">
              <span aria-hidden="true" className={`w-3.5 flex-none font-semibold ${c.was ? 'text-warning' : 'text-success'}`}>{c.was ? '~' : '+'}</span>
              <span className="sr-only">{c.was ? 'Changed' : 'Added'}</span>
              <span className="flex min-w-0 flex-1 flex-col gap-0.5 text-sm [overflow-wrap:anywhere]">
                <Inline text={c.now} />
                {c.was && <span className="text-[12.5px] text-muted">Was {c.was.charAt(0).toLowerCase() + c.was.slice(1)}</span>}
                <span className="text-[12.5px] text-muted">{c.why}</span>
              </span>
              {choice && <Info choices={[choice]} />}
            </li>
          );
        })}
      </ul>
    </section>
  );
}

export function Compatibility({ spec }: { spec: ContractSpec }) {
  const c = spec.compatibility;
  if (!c) return null;
  return (
    <section aria-label="Compatibility" className="flex flex-col gap-3">
      {c.breaking.map((b) => (
        <div key={b.what} className="flex flex-col gap-1.5 rounded-[18px] border border-warning/40 bg-warning-bg px-4 py-3.5">
          <h3 className="m-0 font-sans text-[14.5px] text-warning">Breaks {b.callers.length === 1 ? 'the current caller' : 'current callers'}</h3>
          <p className="m-0 text-sm leading-snug text-ink-2">{b.what}</p>
          <ul className="m-0 flex list-none flex-wrap gap-1.5 p-0">{b.callers.map((n) => <li key={n} className="rounded-full bg-surface px-2.5 py-0.5 text-xs text-ink-2">{n}</li>)}</ul>
        </div>
      ))}
      {c.safe.length > 0 && (
        <div className={`${card} flex flex-col gap-2 px-4 py-3.5`}>
          <h3 className={`m-0 font-sans ${label}`}>Checked against what you asked</h3>
          <ul className="m-0 flex list-none flex-col gap-1.5 p-0 text-sm">{c.safe.map((s) => <li key={s}><span aria-hidden="true" className="mr-2 text-success">✓</span>{s}</li>)}</ul>
        </div>
      )}
    </section>
  );
}

const routes = (ops: Operation[]) => `${ops.length} ${ops.length === 1 ? 'route' : 'routes'}`;
const openapiFile = (doc: Json) => ({ label: 'OpenAPI file', save: () => download(`${toYaml(doc)}\n`, 'application/yaml', 'openapi.yaml') });

function Section({ title, children }: { title: string; children: ReactNode }) {
  return <section className="flex flex-col gap-2"><h4 className={`m-0 font-sans ${label}`}>{title}</h4>{children}</section>;
}

type Ctx = { doc: Json; at: (ptr: string) => Choice[] };

function Schema({ ctx, schema, at }: { ctx: Ctx; schema: Json | undefined; at: string }) {
  const rows = fields(ctx.doc, schema, at);
  return rows.length ? <SchemaTree rows={rows} whyAt={ctx.at} /> : null;
}

function Responses({ o, ctx }: { o: Operation; ctx: Ctx }) {
  const marks = useContext(DiffContext);
  return (
    <Section title="Responses">
      {Object.entries((o.op.responses ?? {}) as Json).map(([code, res]) => {
        const at = `${o.ptr}/responses/${code}`;
        const json = res.content?.['application/json'] as Json | undefined;
        const schema = resolve(ctx.doc, json?.schema, `${at}/content/application~1json/schema`);
        const ok = code.startsWith('2');
        return (
          <div key={code} className="rounded-xl border border-line px-3 py-2.5">
            <div className="flex items-center gap-2">
              <code className={`flex-none rounded-md px-2 py-0.5 font-mono text-xs ${ok ? 'bg-success-bg text-success' : 'bg-danger-bg text-danger'}`}>{code}</code>
              <span className="min-w-0 flex-1 text-[13.5px] [overflow-wrap:anywhere]">{res.description}</span>
              <MarkTag m={marks?.get(at)} />
              <Info choices={ctx.at(at)} />
            </div>
            {schema && <div className="mt-2"><Schema ctx={ctx} schema={schema.schema} at={schema.ptr} /></div>}
          </div>
        );
      })}
    </Section>
  );
}

const example = (o: Operation) => Object.values((o.op.responses ?? {}) as Json).map((r) => (r.content?.['application/json'] as Json | undefined)?.example).find((e) => e !== undefined);

function Meta({ o, spec, ctx }: { o: Operation; spec: ContractSpec; ctx: Ctx }) {
  const scheme = Object.keys((o.op.security?.[0] ?? {}) as Json)[0];
  const auth = (ctx.doc.components?.securitySchemes?.[scheme ?? ''] as Json | undefined)?.description ?? scheme;
  return (
    <dl className="m-0 flex flex-col gap-2 text-[13.5px]">
      {auth && <div className="flex items-center gap-3"><dt className={`${dt} ${label}`}>Auth</dt><dd className="m-0 min-w-0 flex-1 [overflow-wrap:anywhere]">{auth}</dd><Info choices={ctx.at(`${o.ptr}/security`)} /></div>}
      {spec.intent.consumers.length > 0 && <div className="flex items-center gap-3"><dt className={`${dt} ${label}`}>Used by</dt><dd className="m-0">{spec.intent.consumers.join(', ')}</dd></div>}
    </dl>
  );
}

function Detail({ o, spec, ctx }: { o: Operation; spec: ContractSpec; ctx: Ctx }) {
  const params = paramFields(ctx.doc, o);
  const body = resolve(ctx.doc, (o.op.requestBody?.content?.['application/json'] as Json | undefined)?.schema, `${o.ptr}/requestBody/content/application~1json/schema`);
  const eg = example(o);
  return (
    <div className={`${card} flex min-w-0 flex-col gap-4 p-4`}>
      <h3 className="m-0 flex items-center gap-2.5 font-sans">
        <Method m={o.method} /><span className="min-w-0 flex-1 overflow-x-auto font-mono text-[15px] font-normal whitespace-nowrap">{o.path}</span>
        <MarkTag m={useMark(o.ptr)} />
        <Info choices={ctx.at(o.ptr)} />
      </h3>
      {o.op.summary && <div className="text-sm leading-relaxed"><Markdown text={o.op.summary} /></div>}
      <Meta o={o} spec={spec} ctx={ctx} />
      <Section title="Parameters">{params.length ? <SchemaTree rows={params} whyAt={ctx.at} /> : <p className="m-0 text-[13.5px] text-muted">None</p>}</Section>
      {body && <Section title="Request"><Schema ctx={ctx} schema={body.schema} at={body.ptr} /></Section>}
      <Responses o={o} ctx={ctx} />
      {eg !== undefined && <Section title="Example"><pre className="m-0 overflow-x-auto rounded-xl border border-line bg-canvas p-3 font-mono text-xs leading-relaxed whitespace-pre text-ink-2">{JSON.stringify(eg, null, 2)}</pre></Section>}
    </div>
  );
}

// A dot marks a route with anything changed in it.
function Route({ o, doc, current, onPick }: { o: Operation; doc: Json; current: boolean; onPick: () => void }) {
  const changed = useAnyMark(opUnits(doc, o).keys());
  return (
    <button type="button" aria-current={current ? 'true' : undefined} onClick={onPick} className="flex min-h-11 w-full cursor-pointer items-center gap-2.5 rounded-xl border-0 bg-transparent px-3 text-left text-[13px] text-ink-2 aria-[current=true]:bg-soft aria-[current=true]:text-ink">
      <Method m={o.method} /><span className="min-w-0 flex-1 overflow-x-auto whitespace-nowrap font-mono">{o.path}</span>
      {changed && <span role="img" aria-label="Changed" className="h-2 w-2 flex-none rounded-full bg-warning" />}
    </button>
  );
}

// Routes grouped by tag; the selected one is highlighted.
function Routes({ ops, doc, sel, onPick }: { ops: Operation[]; doc: Json; sel: number; onPick: (i: number) => void }) {
  const tagOf = (o: Operation) => (o.op.tags?.[0] as string | undefined) ?? 'Other';
  return (
    <nav aria-label="Routes" className="flex flex-col gap-1">
      {[...new Set(ops.map(tagOf))].map((t) => (
        <div key={t} className="flex flex-col gap-1">
          <span className={`px-3 pt-2 ${label}`}>{t}</span>
          {ops.map((o, i) => tagOf(o) === t && <Route key={o.ptr} o={o} doc={doc} current={i === sel} onPick={() => onPick(i)} />)}
        </div>
      ))}
    </nav>
  );
}

export function download(body: string, type: string, filename: string) {
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([body], { type }));
  a.download = filename;
  a.click();
  URL.revokeObjectURL(a.href);
}

// Links to the spec's own page, where the layout has room; from a session page the hash names the session.
export function FullViewLink({ version }: { version: number }) {
  const ctx = useContext(SessionKeyContext);
  const key = ctx === undefined ? /^#\/s\/([^/?]+)/.exec(location.hash)?.[1] : encodeURIComponent(ctx);
  return key ? <a href={`#/s/${key}/spec?v=${version}`} aria-label="Open full view" title="Open full view" className={round}>{svg('M14 4h6v6M10 20H4v-6M20 4l-7 7M4 20l7-7')}</a> : null;
}

export const decisionCount = (spec: ContractSpec) => `${spec.choices.length} ${spec.choices.length === 1 ? 'decision' : 'decisions'} · ${spec.choices.filter((c) => c.why.some((w) => w.source === 'answer')).length} answered by you`;

export type ViewProps = { spec: ContractSpec; full?: boolean; verdict?: Verdict | null };

// A passed double-check is a tick beside the title; the decisions no element carries sit there too.
export function Header({ spec, count, full, file, verdict, info = [] }: { spec: ContractSpec; count: string; full: boolean; file: { label: string; save: () => void }; verdict?: Verdict | null; info?: Choice[] }) {
  return (
    <header className="flex flex-wrap items-center gap-3">
      <div className="flex min-w-0 flex-1 basis-48 flex-col">
        <h2 className="m-0 flex items-center gap-2 text-xl [overflow-wrap:anywhere]">
          <span className="min-w-0">{spec.title}</span>
          <MarkTag m={useMark('#title')} />
          {verdict?.passed && <StatusIcon verdict={verdict} />}
          <Info choices={info} label={info.length === 1 ? undefined : `Why: ${info.length} decisions about the whole contract`} />
        </h2>
        <span className="text-xs text-muted">{count} · version {spec.version}</span>
      </div>
      {!full && <FullViewLink version={spec.version} />}
      <button type="button" aria-label={`Download ${file.label}`} title={`Download ${file.label}`} onClick={file.save} className={round}>{svg('M12 4v11M7 10l5 5 5-5M5 20h14')}</button>
    </header>
  );
}

export function Purpose({ spec }: { spec: ContractSpec }) {
  const m = useMark('#purpose');
  return (
    <section aria-label="What it does" className={`${card} flex flex-col gap-1.5 px-4 py-3.5 ${m ? MARKED[m] : ''}`}>
      <h3 className={`m-0 flex items-center gap-2 font-sans ${label}`}>What it does<MarkTag m={m} /></h3>
      <div className="text-[14.5px] leading-relaxed"><Markdown text={spec.intent.purpose} /></div>
    </section>
  );
}

// The ledger card: what the spec is and its routes; detail and decisions live on the full page.
function CardView({ spec, ops, doc, verdict }: { spec: ContractSpec; ops: Operation[]; doc: Json; verdict?: Verdict | null }) {
  return (
    <div className="@container flex min-w-0 flex-col gap-4">
      <Header spec={spec} count={routes(ops)} full={false} file={openapiFile(doc)} verdict={verdict} />
      <Purpose spec={spec} />
      <ul aria-label="Routes" className="m-0 flex list-none flex-col gap-1.5 p-0">
        {ops.map((o) => <li key={o.ptr} className="flex items-center gap-2.5 text-[13px]"><Method m={o.method} /><span className="min-w-0 flex-1 overflow-x-auto whitespace-nowrap font-mono">{o.path}</span></li>)}
      </ul>
      <p className="m-0 text-[13px] text-muted">{decisionCount(spec)}</p>
    </div>
  );
}

export function RestView({ spec, full = false, verdict }: ViewProps) {
  const [sel, setSel] = useState(0);
  const doc = spec.document as Json;
  const ops = operations(doc);
  if (!full) return <CardView spec={spec} ops={ops} doc={doc} verdict={verdict} />;
  const ctx: Ctx = { doc, at: (p) => choicesAt(spec.choices, p) };
  const o = ops[sel];
  const many = ops.length > 1;
  return (
    <div className="@container flex min-w-0 flex-col gap-4">
      <Header spec={spec} count={routes(ops)} full file={openapiFile(doc)} verdict={verdict} info={unplaced(spec.choices, restPointers(doc, ops))} />
      <Changes spec={spec} />
      <Compatibility spec={spec} />
      <Purpose spec={spec} />
      <div className={`grid grid-cols-1 items-start gap-4 ${many ? '@[900px]:grid-cols-[280px_minmax(0,1fr)]' : ''}`}>
        {many && <Routes ops={ops} doc={doc} sel={sel} onPick={setSel} />}
        {o && <Detail o={o} spec={spec} ctx={ctx} />}
      </div>
    </div>
  );
}
