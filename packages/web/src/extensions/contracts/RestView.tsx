import { useEffect, useState, type ReactNode } from 'react';
import { Markdown } from '../../Markdown.tsx';
import { StatusIcon } from '../../StatusIcon.tsx';
import type { Verdict } from '../../api.ts';
import { opUnits, useAnyMark, useMark } from './diff.ts';
import { choicesAt, Info, unplaced } from './Info.tsx';
import { Changes, Glyph, MoreAbout, Purpose, ShortChange, useFlash, useItem, useReview } from './Review.tsx';
import { MarkTag, SchemaTree } from './SchemaTree.tsx';
import { card, label } from './Sections.tsx';

export { card, label };
import { fields, operations, paramFields, resolve, restPointers, toYaml, type Choice, type ContractSpec, type Json, type Operation } from './spec.ts';

const METHOD: Record<string, string> = { GET: 'bg-link-bg text-link', POST: 'bg-success-bg text-success', DELETE: 'bg-danger-bg text-danger' };
const dt = 'w-20 flex-none whitespace-nowrap';
// The pane header's actions: one 44px icon button each, named by aria-label and title.
const round = 'grid h-11 w-11 flex-none cursor-pointer place-items-center rounded-full border-0 bg-transparent text-ink-2 no-underline hover:bg-soft hover:text-ink focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-link';
const svg = (d: string) => <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={d} /></svg>;

function Method({ m }: { m: string }) {
  return <span className={`flex-none rounded-md px-2 py-0.5 font-mono text-xs font-semibold ${METHOD[m] ?? 'bg-warning-bg text-warning'}`}>{m}</span>;
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

function Response({ o, code, res, ctx }: { o: Operation; code: string; res: Json; ctx: Ctx }) {
  const at = `${o.ptr}/responses/${code}`;
  const json = res.content?.['application/json'] as Json | undefined;
  const schema = resolve(ctx.doc, json?.schema, `${at}/content/application~1json/schema`);
  const m = useMark(at), item = useItem(at);
  const flash = useFlash<HTMLDivElement>(at);
  return (
    <div ref={flash.ref} tabIndex={-1} data-ptr={at} className={`flex scroll-mt-16 flex-col gap-2 rounded-xl border border-line px-3 py-2.5 outline-none ${flash.cls}`}>
      <div className="flex items-center gap-2">
        <code className={`flex-none rounded-md px-2 py-0.5 font-mono text-xs ${code.startsWith('2') ? 'bg-success-bg text-success' : 'bg-danger-bg text-danger'}`}>{code}</code>
        <span className="min-w-0 flex-1 text-[13.5px] [overflow-wrap:anywhere]">{res.description}</span>
        <MarkTag m={m} />
        <Info choices={ctx.at(at)} />
      </div>
      {item && <ShortChange item={item} />}
      {schema && <Schema ctx={ctx} schema={schema.schema} at={schema.ptr} />}
    </div>
  );
}

function Responses({ o, ctx }: { o: Operation; ctx: Ctx }) {
  return <Section title="Responses">{Object.entries((o.op.responses ?? {}) as Json).map(([code, res]) => <Response key={code} o={o} code={code} res={res} ctx={ctx} />)}</Section>;
}

const example = (o: Operation) => Object.values((o.op.responses ?? {}) as Json).map((r) => (r.content?.['application/json'] as Json | undefined)?.example).find((e) => e !== undefined);

function Meta({ o, spec, ctx }: { o: Operation; spec: ContractSpec; ctx: Ctx }) {
  const scheme = Object.keys((o.op.security?.[0] ?? {}) as Json)[0];
  const auth = (ctx.doc.components?.securitySchemes?.[scheme ?? ''] as Json | undefined)?.description ?? scheme;
  return (
    <dl className="m-0 flex flex-col gap-2 text-[13.5px]">
      {auth && <div className="flex items-center gap-3"><dt className={`${dt} ${label}`}>Auth</dt><dd className="m-0 min-w-0 flex-1 [overflow-wrap:anywhere]">{auth}</dd><Info choices={ctx.at(`${o.ptr}/security`)} /></div>}
      {spec.intent.consumers.length > 0 && <div className="flex items-center gap-3"><dt className={`${dt} ${label}`}>Used by</dt><dd className="m-0 min-w-0 flex-1 [overflow-wrap:anywhere]">{spec.intent.consumers.join(', ')}</dd></div>}
    </dl>
  );
}

function Detail({ o, spec, ctx }: { o: Operation; spec: ContractSpec; ctx: Ctx }) {
  const params = paramFields(ctx.doc, o);
  const body = resolve(ctx.doc, (o.op.requestBody?.content?.['application/json'] as Json | undefined)?.schema, `${o.ptr}/requestBody/content/application~1json/schema`);
  const eg = example(o);
  const item = useItem(o.ptr);
  const flash = useFlash<HTMLDivElement>(o.ptr);
  return (
    <div ref={flash.ref} tabIndex={-1} data-ptr={o.ptr} className={`${card} flex min-w-0 scroll-mt-16 flex-col gap-4 p-4 outline-none ${flash.cls}`}>
      <h3 className="m-0 flex items-center gap-2.5 font-sans">
        <Method m={o.method} /><span className="min-w-0 flex-1 overflow-x-auto font-mono text-[15px] font-normal whitespace-nowrap">{o.path}</span>
        <MarkTag m={useMark(o.ptr)} />
        <Info choices={ctx.at(o.ptr)} />
      </h3>
      {item && <ShortChange item={item} />}
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
      {changed && <span role="img" aria-label="Changed" className="inline-flex"><Glyph m="changed" /></span>}
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

export type ViewProps = { spec: ContractSpec; verdict?: Verdict | null };

// A passed double-check is a tick beside the title; the decisions no element carries sit there too.
export function Header({ spec, count, file, verdict, info = [] }: { spec: ContractSpec; count: string; file: { label: string; save: () => void }; verdict?: Verdict | null; info?: Choice[] }) {
  const flash = useFlash<HTMLElement>('#title');
  return (
    <header ref={flash.ref} tabIndex={-1} data-ptr="#title" className={`flex flex-wrap items-center gap-3 rounded-lg outline-none ${flash.cls}`}>
      <div className="flex min-w-0 flex-1 basis-48 flex-col">
        <h2 className="m-0 flex items-center gap-2 font-sans text-[17px] leading-snug font-semibold [overflow-wrap:anywhere]">
          {/* The session header carries the full title, so a phone keeps this one to two lines. */}
          <span title={spec.title} className="min-w-0 max-sm:line-clamp-2">{spec.title}</span>
          <MarkTag m={useMark('#title')} />
          {verdict?.passed && <StatusIcon verdict={verdict} />}
          <Info choices={info} label={info.length === 1 ? undefined : `Why: ${info.length} decisions about the whole contract`} />
        </h2>
        <span className="text-xs text-muted">{count} · version {spec.version}</span>
      </div>
      <button type="button" aria-label={`Download ${file.label}`} title={`Download ${file.label}`} onClick={file.save} className={round}>{svg('M12 4v11M7 10l5 5 5-5M5 20h14')}</button>
    </header>
  );
}

export function RestView({ spec, verdict }: ViewProps) {
  const [sel, setSel] = useState(0);
  const doc = spec.document as Json;
  const ops = operations(doc);
  const ctx: Ctx = { doc, at: (p) => choicesAt(spec.choices, p) };
  const { flash } = useReview();
  // A jump to a change opens the route it is in; the changed element then scrolls itself into view.
  useEffect(() => {
    const i = flash ? ops.findIndex((x) => opUnits(doc, x).has(flash.ptr)) : -1;
    if (i >= 0) setSel(i);
  }, [flash]);
  const o = ops[sel];
  const many = ops.length > 1;
  return (
    <div className="@container flex min-w-0 flex-col gap-6">
      <Header spec={spec} count={routes(ops)} file={openapiFile(doc)} verdict={verdict} info={unplaced(spec.choices, restPointers(doc, ops))} />
      <Purpose spec={spec} />
      <Changes spec={spec} />
      <div className={`grid grid-cols-1 items-start gap-4 ${many ? '@[900px]:grid-cols-[280px_minmax(0,1fr)]' : ''}`}>
        {many && <Routes ops={ops} doc={doc} sel={sel} onPick={setSel} />}
        {o && <Detail o={o} spec={spec} ctx={ctx} />}
      </div>
      <MoreAbout spec={spec} />
    </div>
  );
}
