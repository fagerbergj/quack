import { useContext, useState, type ReactNode } from 'react';
import { SessionKeyContext } from '../../extension.ts';
import { Markdown } from '../../Markdown.tsx';
import { SchemaTree, WhyChip } from './SchemaTree.tsx';
import { fields, operations, resolve, toYaml, type Choice, type ContractSpec, type Field, type Json, type Operation } from './spec.ts';

const METHOD: Record<string, string> = { GET: 'bg-link-bg text-link', POST: 'bg-success-bg text-success', DELETE: 'bg-danger-bg text-danger' };
const SOURCE: Record<Choice['why'][number]['source'], { label: string; cls: string }> = {
  answer: { label: 'You', cls: 'text-warning' },
  guide: { label: 'Guide', cls: 'text-accent-ink' },
  code: { label: 'Existing', cls: 'text-link' },
};
// An answer's `ref` is the question as the user saw it and its `text` their answer.
const gist = (w: Choice['why'][number]) => (w.source === 'answer' ? `You answered “${w.ref}”: ${w.text}` : w.text);
const label = 'text-xs font-semibold uppercase tracking-wider text-muted';
const dt = 'w-20 flex-none whitespace-nowrap';
const card = 'rounded-[18px] border border-line bg-surface';
const pill = 'inline-flex min-h-11 flex-none cursor-pointer items-center rounded-full border border-line-strong bg-transparent px-4 text-[13.5px] text-ink-2 hover:text-ink';

// Decision and why text is Markdown-flavoured prose; only `code` spans are rendered, inline.
function Inline({ text }: { text: string }) {
  return <>{text.split(/`([^`]+)`/).map((t, i) => (i % 2 ? <code key={i} className="rounded bg-soft px-1 font-mono text-[0.92em]">{t}</code> : t))}</>;
}

function Method({ m }: { m: string }) {
  return <span className={`flex-none rounded-md px-2 py-0.5 font-mono text-xs font-semibold ${METHOD[m] ?? 'bg-warning-bg text-warning'}`}>{m}</span>;
}

// The reasons behind one choice, each tagged by where it came from; sticky so it stays in view while the page scrolls.
function WhyPanel({ choice, onClose }: { choice: Choice; onClose: () => void }) {
  return (
    <section role="region" aria-label={`Why: ${choice.decision}`} onKeyDown={(e) => { if (e.key === 'Escape') { e.stopPropagation(); onClose(); } }} className="sticky bottom-0 z-10 flex flex-col gap-3 rounded-[22px] border border-line-strong bg-surface p-4 shadow-lift @3xl:top-0 @3xl:bottom-auto">
      <div className="flex items-center gap-2">
        <h3 className="m-0 min-w-0 flex-1 text-[17px] [overflow-wrap:anywhere]"><Inline text={choice.decision} /></h3>
        <button type="button" aria-label="Close" autoFocus onClick={onClose} className="-mr-2 grid h-11 w-11 flex-none cursor-pointer place-items-center rounded-full border-0 bg-transparent text-muted hover:text-ink">
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18" /></svg>
        </button>
      </div>
      <ul className="m-0 flex max-h-[40vh] @3xl:max-h-none list-none flex-col gap-2 overflow-y-auto p-0">
        {choice.why.map((w, i) => (
          <li key={i} className="flex gap-3 rounded-2xl border border-line bg-canvas px-3.5 py-3 text-sm leading-snug">
            <span className={`w-16 flex-none text-xs font-semibold ${SOURCE[w.source].cls}`}>{SOURCE[w.source].label}</span>
            {w.source === 'answer' ? (
              <span className="flex min-w-0 flex-1 flex-col gap-1 [overflow-wrap:anywhere]">
                <span className="text-muted">Answered “{w.ref}”</span>
                <strong className="font-semibold"><Inline text={w.text} /></strong>
              </span>
            ) : (
              <span className="flex min-w-0 flex-1 flex-col gap-1 [overflow-wrap:anywhere]">
                <Inline text={w.text} />
                <code className="font-mono text-xs text-muted">{w.ref}</code>
              </span>
            )}
          </li>
        ))}
      </ul>
    </section>
  );
}

function Changes({ spec, onWhy }: { spec: ContractSpec; onWhy: (c: Choice) => void }) {
  const byId = new Map(spec.choices.map((c) => [c.id, c]));
  if (!spec.changes?.length) return null;
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
              {choice && <button type="button" aria-label={`Why: ${choice.decision}`} onClick={() => onWhy(choice)} className="-my-1 -mr-2 min-h-11 flex-none cursor-pointer border-0 bg-transparent px-2 text-[13px] text-link">Why ›</button>}
            </li>
          );
        })}
      </ul>
    </section>
  );
}

function Compatibility({ spec }: { spec: ContractSpec }) {
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

function Section({ title, children }: { title: string; children: ReactNode }) {
  return <section className="flex flex-col gap-2"><h4 className={`m-0 font-sans ${label}`}>{title}</h4>{children}</section>;
}

type Ctx = { doc: Json; whyAt: (ptr: string) => Choice | undefined; onWhy: (c: Choice) => void };

function Schema({ ctx, schema, at }: { ctx: Ctx; schema: Json | undefined; at: string }) {
  const rows = fields(ctx.doc, schema, at);
  return rows.length ? <SchemaTree rows={rows} whyAt={ctx.whyAt} onWhy={ctx.onWhy} /> : null;
}

function Responses({ o, ctx }: { o: Operation; ctx: Ctx }) {
  return (
    <Section title="Responses">
      {Object.entries((o.op.responses ?? {}) as Json).map(([code, res]) => {
        const at = `${o.ptr}/responses/${code}`;
        const choice = ctx.whyAt(at);
        const json = res.content?.['application/json'] as Json | undefined;
        const schema = resolve(ctx.doc, json?.schema, `${at}/content/application~1json/schema`);
        const ok = code.startsWith('2');
        return (
          <div key={code} className="rounded-xl border border-line px-3 py-2.5">
            <div className="flex items-center gap-2">
              <code className={`flex-none rounded-md px-2 py-0.5 font-mono text-xs ${ok ? 'bg-success-bg text-success' : 'bg-danger-bg text-danger'}`}>{code}</code>
              <span className="min-w-0 flex-1 text-[13.5px] [overflow-wrap:anywhere]">{res.description}</span>
              {choice && <WhyChip choice={choice} onWhy={ctx.onWhy} />}
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
  const choice = ctx.whyAt(`${o.ptr}/security`);
  return (
    <dl className="m-0 flex flex-col gap-2 text-[13.5px]">
      {auth && <div className="flex items-center gap-3"><dt className={`${dt} ${label}`}>Auth</dt><dd className="m-0 min-w-0 flex-1 [overflow-wrap:anywhere]">{auth}</dd>{choice && <WhyChip choice={choice} onWhy={ctx.onWhy} />}</div>}
      {spec.intent.consumers.length > 0 && <div className="flex items-center gap-3"><dt className={`${dt} ${label}`}>Used by</dt><dd className="m-0">{spec.intent.consumers.join(', ')}</dd></div>}
    </dl>
  );
}

function Detail({ o, spec, ctx }: { o: Operation; spec: ContractSpec; ctx: Ctx }) {
  const params: Field[] = ((o.op.parameters ?? []) as Json[]).map((p, i) => ({ name: p.name, type: p.schema?.type ?? 'string', required: Boolean(p.required), desc: p.description, depth: 0, ptr: `${o.ptr}/parameters/${i}`, where: p.in, facts: [] }));
  const body = resolve(ctx.doc, (o.op.requestBody?.content?.['application/json'] as Json | undefined)?.schema, `${o.ptr}/requestBody/content/application~1json/schema`);
  const eg = example(o);
  return (
    <div className={`${card} flex min-w-0 flex-col gap-4 p-4`}>
      <h3 className="m-0 flex items-center gap-2.5 font-sans">
        <Method m={o.method} /><span className="min-w-0 flex-1 overflow-x-auto font-mono text-[15px] font-normal whitespace-nowrap">{o.path}</span>
      </h3>
      {o.op.summary && <div className="text-sm leading-relaxed"><Markdown text={o.op.summary} /></div>}
      <Meta o={o} spec={spec} ctx={ctx} />
      <Section title="Parameters">{params.length ? <SchemaTree rows={params} whyAt={ctx.whyAt} onWhy={ctx.onWhy} /> : <p className="m-0 text-[13.5px] text-muted">None</p>}</Section>
      {body && <Section title="Request"><Schema ctx={ctx} schema={body.schema} at={body.ptr} /></Section>}
      <Responses o={o} ctx={ctx} />
      {eg !== undefined && <Section title="Example"><pre className="m-0 overflow-x-auto rounded-xl border border-line bg-canvas p-3 font-mono text-xs leading-relaxed whitespace-pre text-ink-2">{JSON.stringify(eg, null, 2)}</pre></Section>}
    </div>
  );
}

// Routes grouped by tag; the selected one is highlighted.
function Routes({ ops, sel, onPick }: { ops: Operation[]; sel: number; onPick: (i: number) => void }) {
  const tagOf = (o: Operation) => (o.op.tags?.[0] as string | undefined) ?? 'Other';
  return (
    <nav aria-label="Routes" className="flex flex-col gap-1">
      {[...new Set(ops.map(tagOf))].map((t) => (
        <div key={t} className="flex flex-col gap-1">
          <span className={`px-3 pt-2 ${label}`}>{t}</span>
          {ops.map((o, i) => tagOf(o) === t && (
            <button key={o.ptr} type="button" aria-current={i === sel ? 'true' : undefined} onClick={() => onPick(i)} className="flex min-h-11 w-full cursor-pointer items-center gap-2.5 rounded-xl border-0 bg-transparent px-3 text-left text-[13px] text-ink-2 aria-[current=true]:bg-soft aria-[current=true]:text-ink">
              <Method m={o.method} /><span className="min-w-0 flex-1 overflow-x-auto whitespace-nowrap font-mono">{o.path}</span>
            </button>
          ))}
        </div>
      ))}
    </nav>
  );
}

// Every decision in one list, so each has a way to its reasons even when no field on screen carries its chip.
function Decisions({ spec, onWhy }: { spec: ContractSpec; onWhy: (c: Choice) => void }) {
  return (
    <section aria-label="Decisions" className={`${card} overflow-hidden`}>
      <h3 className={`m-0 px-4 py-3 font-sans ${label}`}>Decisions</h3>
      <ul className="m-0 list-none p-0">
        {spec.choices.map((c) => (
          <li key={c.id} className="flex items-center gap-3 border-t border-line pl-4">
            <span className="flex min-w-0 flex-1 flex-col gap-0.5 py-2.5 text-sm [overflow-wrap:anywhere]"><Inline text={c.decision} /><span className="line-clamp-2 text-[12.5px] text-muted">{c.why[0] && <Inline text={gist(c.why[0])} />}</span></span>
            <button type="button" aria-label={`Why: ${c.decision}`} onClick={() => onWhy(c)} className="min-h-11 flex-none cursor-pointer border-0 bg-transparent px-4 text-[13px] text-link">Why ›</button>
          </li>
        ))}
      </ul>
    </section>
  );
}

function download(doc: unknown) {
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([`${toYaml(doc)}\n`], { type: 'application/yaml' }));
  a.download = 'openapi.yaml';
  a.click();
  URL.revokeObjectURL(a.href);
}

// Links to the spec's own page, where the three-column layout has room; from a session page the hash names the session.
function FullViewLink({ version }: { version: number }) {
  const ctx = useContext(SessionKeyContext);
  const key = ctx === undefined ? /^#\/s\/([^/?]+)/.exec(location.hash)?.[1] : encodeURIComponent(ctx);
  return key ? <a href={`#/s/${key}/spec?v=${version}`} className={`${pill} no-underline`}>Open full view</a> : null;
}

const decisionCount = (spec: ContractSpec) => `${spec.choices.length} ${spec.choices.length === 1 ? 'decision' : 'decisions'} · ${spec.choices.filter((c) => c.why.some((w) => w.source === 'answer')).length} answered by you`;

function Header({ spec, count, full, doc }: { spec: ContractSpec; count: number; full: boolean; doc: Json }) {
  return (
    <header className="flex flex-wrap items-center gap-3">
      <div className="flex min-w-0 flex-1 basis-48 flex-col">
        <h2 className="m-0 text-xl [overflow-wrap:anywhere]">{spec.title}</h2>
        <span className="text-xs text-muted">{count} {count === 1 ? 'route' : 'routes'} · version {spec.version}</span>
      </div>
      {!full && <FullViewLink version={spec.version} />}
      <button type="button" onClick={() => download(doc)} className={pill}>OpenAPI file</button>
    </header>
  );
}

function Purpose({ spec }: { spec: ContractSpec }) {
  return (
    <section aria-label="What it does" className={`${card} flex flex-col gap-1.5 px-4 py-3.5`}>
      <h3 className={`m-0 font-sans ${label}`}>What it does</h3>
      <div className="text-[14.5px] leading-relaxed"><Markdown text={spec.intent.purpose} /></div>
    </section>
  );
}

// The ledger card: what the spec is and its routes; detail and decisions live on the full page.
function CardView({ spec, ops, doc }: { spec: ContractSpec; ops: Operation[]; doc: Json }) {
  return (
    <div className="@container flex min-w-0 flex-col gap-4">
      <Header spec={spec} count={ops.length} full={false} doc={doc} />
      <Purpose spec={spec} />
      <ul aria-label="Routes" className="m-0 flex list-none flex-col gap-1.5 p-0">
        {ops.map((o) => <li key={o.ptr} className="flex items-center gap-2.5 text-[13px]"><Method m={o.method} /><span className="min-w-0 flex-1 overflow-x-auto whitespace-nowrap font-mono">{o.path}</span></li>)}
      </ul>
      <p className="m-0 text-[13px] text-muted">{decisionCount(spec)}</p>
    </div>
  );
}

export function RestView({ spec, full = false }: { spec: ContractSpec; full?: boolean }) {
  const [sel, setSel] = useState(0);
  const [why, setWhy] = useState<Choice>();
  const doc = spec.document as Json;
  const ops = operations(doc);
  if (!full) return <CardView spec={spec} ops={ops} doc={doc} />;
  const ctx: Ctx = { doc, whyAt: (p) => spec.choices.find((c) => c.pointer === p), onWhy: setWhy };
  const o = ops[sel];
  const many = ops.length > 1;
  return (
    <div className="@container flex min-w-0 flex-col gap-4">
      <Header spec={spec} count={ops.length} full doc={doc} />
      <Changes spec={spec} onWhy={setWhy} />
      <Compatibility spec={spec} />
      <Purpose spec={spec} />
      <div className={`grid grid-cols-1 items-start gap-4 ${many ? '@[1100px]:grid-cols-[280px_minmax(0,1fr)_360px]' : '@[1100px]:grid-cols-[minmax(0,1fr)_360px]'}`}>
        {many && <Routes ops={ops} sel={sel} onPick={setSel} />}
        {o && <Detail o={o} spec={spec} ctx={ctx} />}
        <div className="flex min-w-0 flex-col gap-4">
          {why && <WhyPanel choice={why} onClose={() => setWhy(undefined)} />}
          <Decisions spec={spec} onWhy={setWhy} />
        </div>
      </div>
    </div>
  );
}
