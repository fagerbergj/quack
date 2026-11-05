import { Graph, layout, type EdgeLabel, type GraphLabel, type NodeLabel } from '@dagrejs/dagre';
import { useMemo } from 'react';
import type { Decl } from './spec.ts';

// Monospace at 12px; boxes are sized from character counts, since text cannot be measured before it is drawn.
const CH = 7.25, ROW = 18, HEAD = 34, PAD = 10, MAX_ROWS = 8, MAX_CHARS = 46;
const clip = (s: string) => (s.length > MAX_CHARS ? `${s.slice(0, MAX_CHARS - 1)}…` : s);

export type Box = { decl: Decl; x: number; y: number; w: number; h: number; lines: Array<[string, string]> };
export type Diagram = { width: number; height: number; boxes: Box[]; edges: Array<{ from: string; to: string; d: string }> };

const DRAWN = new Set<Decl['kind']>(['type', 'interface', 'enum', 'other']);

// A box lists the top-level fields (a union's variants as `| …`), or the aliased type when there are none.
function lines(d: Decl): Array<[string, string]> {
  const top = d.rows.filter((r) => r.depth === 0).map((r): [string, string] => {
    if (r.where === 'one of') return [`| ${r.name}`, ''];
    return d.kind === 'enum' ? [r.name, ` = ${r.type}`] : [`${r.name}${r.required ? '' : '?'}`, `: ${r.type}`];
  });
  if (!top.length && d.text) return [[clip(`= ${d.text}`), '']];
  const shown = top.slice(0, MAX_ROWS).map(([n, t]): [string, string] => [clip(n), clip(n + t).slice(clip(n).length)]);
  return top.length > MAX_ROWS ? [...shown, [`+ ${top.length - MAX_ROWS} more`, '']] : shown;
}

/** Lays out a code contract's types as a class diagram: one box per type, an arrow from each type to the types it names. */
export function classDiagram(decls: Decl[]): Diagram {
  const types = decls.filter((d) => DRAWN.has(d.kind));
  const names = new Set(types.map((d) => d.name));
  const g = new Graph<GraphLabel, NodeLabel, EdgeLabel>();
  g.setGraph({ rankdir: 'LR', nodesep: 20, ranksep: 56, marginx: 4, marginy: 4 });
  g.setDefaultEdgeLabel(() => ({}));
  for (const d of types) {
    const ls = lines(d), title = `${d.name}${d.generics ?? ''}`;
    const w = Math.max(title.length + d.kind.length + 4, ...ls.map(([n, t]) => n.length + t.length)) * CH + 2 * PAD;
    g.setNode(d.name, { width: w, height: HEAD + ls.length * ROW + PAD });
  }
  for (const d of types) for (const u of d.uses) if (names.has(u)) g.setEdge(d.name, u);
  layout(g);
  const boxes = types.map((decl) => {
    const n = g.node(decl.name);
    return { decl, x: n.x! - n.width / 2, y: n.y! - n.height / 2, w: n.width, h: n.height, lines: lines(decl) };
  });
  const edges = g.edges().map((e) => ({ from: e.v, to: e.w, d: (g.edge(e).points ?? []).map((p, i) => `${i ? 'L' : 'M'}${p.x.toFixed(1)} ${p.y.toFixed(1)}`).join(' ') }));
  const { width = 0, height = 0 } = g.graph();
  return { width, height, boxes, edges };
}

function TypeBox({ b, onJump }: { b: Box; onJump: (name: string) => void }) {
  const go = () => onJump(b.decl.name);
  return (
    <g role="button" tabIndex={0} aria-label={`${b.decl.name}, go to its declaration`} onClick={go} onKeyDown={(e) => (e.key === 'Enter' || e.key === ' ') && (e.preventDefault(), go())} className="cursor-pointer outline-none [&:focus-visible>rect]:stroke-link [&:hover>rect]:stroke-ink-2">
      <rect x={b.x} y={b.y} width={b.w} height={b.h} rx={8} strokeDasharray={b.decl.exported ? undefined : '4 3'} className="fill-surface stroke-line" strokeWidth={1.25} />
      <rect x={b.x} y={b.y} width={b.w} height={HEAD - 8} rx={8} className="fill-soft" />
      <text x={b.x + PAD} y={b.y + 17} className="fill-ink font-mono text-[12px] font-semibold">
        {b.decl.name}<tspan className="fill-muted font-normal">{b.decl.generics}</tspan>
        <tspan dx={8} className="fill-muted text-[10px] font-normal tracking-wide uppercase">{b.decl.kind === 'other' ? '' : b.decl.kind}</tspan>
      </text>
      {b.lines.map(([n, t], i) => (
        <text key={i} x={b.x + PAD} y={b.y + HEAD + 4 + i * ROW} className="fill-ink-2 font-mono text-[12px]">
          {n}<tspan className="fill-success">{t}</tspan>
        </text>
      ))}
    </g>
  );
}

/** The contract's types and how they reference each other; a box opens its declaration below. Functions and constants are listed, not drawn. */
export function ClassDiagram({ decls, onJump }: { decls: Decl[]; onJump: (name: string) => void }) {
  const dg = useMemo(() => classDiagram(decls), [decls]);
  if (!dg.boxes.length) return null;
  return (
    <section aria-label="Class diagram" className="min-w-0 rounded-[18px] border border-line bg-surface p-4">
      <h2 className="m-0 mb-3 text-[15px] font-semibold">Class diagram</h2>
      <div className="max-h-[70vh] overflow-auto rounded-xl border border-line bg-canvas">
        <svg width={dg.width} height={dg.height} viewBox={`0 0 ${dg.width} ${dg.height}`} className="block">
          <defs>
            <marker id="cd-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
              <path d="M0 0L10 5L0 10z" className="fill-muted" />
            </marker>
          </defs>
          {dg.edges.map((e) => <path key={`${e.from}>${e.to}`} d={e.d} fill="none" strokeWidth={1.25} markerEnd="url(#cd-arrow)" className="stroke-muted"><title>{`${e.from} uses ${e.to}`}</title></path>)}
          {dg.boxes.map((b) => <TypeBox key={b.decl.name} b={b} onJump={onJump} />)}
        </svg>
      </div>
      <p className="m-0 mt-2 text-xs text-muted">An arrow points from a type to a type it uses. A dashed box is not exported.</p>
    </section>
  );
}
