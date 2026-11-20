import { Background, Controls, Handle, MarkerType, MiniMap, Position, ReactFlow, type Edge, type Node, type NodeProps } from '@xyflow/react';
import '@xyflow/react/dist/style.css';
import ELK, { type ElkNode } from 'elkjs/lib/elk.bundled.js';
import { createContext, useContext, useEffect, useId, useMemo, useRef, useState, type FocusEvent, type PointerEvent, type RefObject } from 'react';
import { createPortal } from 'react-dom';
import { useMedia } from '../../useDesktop.ts';
import { aimAt, defaultFocus, focusView, type Box, type Focused } from './class-model.ts';
import { CommentGlyph, commentsUnder, docTarget, StripComment, useComments } from './Comments.tsx';
import { DiffContext, type Mark } from './diff.ts';
import { Glyph, useReview } from './Review.tsx';
import { itemsUnder, lede, worst, type Item } from './review.ts';
import type { Decl } from './spec.ts';

// Monospace at 12px; boxes are sized from character counts, since ELK needs sizes before anything is drawn.
const CH = 7.25, ROW = 22, HEAD = 32, PAD = 10, MAX_CHARS = 48;
const clip = (s: string) => (s.length > MAX_CHARS ? `${s.slice(0, MAX_CHARS - 1)}…` : s);
const rowText = (r: Box['rows'][number]) => `${r.name} ${clip(r.type)}`;
const size = (b: Box) => ({
  width: Math.ceil(Math.max(b.decl.name.length + (b.decl.generics?.length ?? 0) + b.decl.kind.length + 8, ...b.rows.map((r) => rowText(r).length + r.chips.join('  ').length + 2 * r.chips.length)) * CH + 2 * PAD + 12),
  height: HEAD + b.rows.length * ROW + 8,
});

const elk = new ELK();

/** Places the boxes left to right with ELK's layered algorithm: one port per field that holds an arrow, in field order, so arrows leave from their field. */
async function place(view: Focused): Promise<{ at: Map<string, { x: number; y: number }>; width: number; height: number }> {
  const graph: ElkNode = {
    id: 'root',
    layoutOptions: { 'elk.algorithm': 'layered', 'elk.direction': 'RIGHT', 'elk.edgeRouting': 'ORTHOGONAL', 'elk.spacing.nodeNode': '28', 'elk.layered.spacing.nodeNodeBetweenLayers': '64' },
    children: view.boxes.map((b) => ({
      id: b.decl.name, ...size(b),
      layoutOptions: { 'elk.portConstraints': 'FIXED_ORDER' },
      ports: [{ id: `${b.decl.name}:in`, layoutOptions: { 'elk.port.side': 'WEST' } }, ...b.rows.flatMap((r, i) => (r.edges.length ? [{ id: `${b.decl.name}:${i}`, layoutOptions: { 'elk.port.side': 'EAST', 'elk.port.index': String(i) } }] : []))],
    })),
    edges: view.edges.map((e) => ({ id: e.id, sources: [`${e.source}:${e.row}`], targets: [`${e.target}:in`] })),
  };
  const laid = await elk.layout(graph);
  return { at: new Map((laid.children ?? []).map((c) => [c.id, { x: c.x ?? 0, y: c.y ?? 0 }])), width: laid.width ?? 0, height: laid.height ?? 0 };
}

type Act = { pick: (name: string) => void; focus: (name: string) => void; tip: (name?: string, el?: HTMLElement) => void; sel?: string; tipId: string; tipFor?: string };
const Acts = createContext<Act>({ pick: () => {}, focus: () => {}, tip: () => {}, tipId: '' });
type BoxNode = Node<{ box: Box }, 'box'>;

const chip = 'nodrag inline-flex cursor-pointer items-center gap-1 rounded border-0 bg-link-bg px-1 font-mono text-[11px] text-link hover:underline focus-ring';
const BORDER: Record<Mark, string> = { added: 'border-2 border-warning', changed: 'border-2 border-warning', breaking: 'border-2 border-danger' };

/** How a box or chip's changes read to a screen reader, after its name. */
export function changeWords(items: Item[]) {
  if (!items.length) return '';
  const breaks = items.filter((i) => i.breaking).length;
  return `, ${items.length} ${items.length === 1 ? 'change' : 'changes'}${breaks ? `, ${breaks === items.length ? (breaks === 1 ? 'it breaks' : 'they break') : `${breaks} ${breaks === 1 ? 'breaks' : 'break'}`} a caller` : ''}`;
}

// A row stands for its field and, for an inline object, the fields nested under it.
function rowMark(marks: Map<string, Mark> | undefined, ptr?: string): Mark | undefined {
  if (!marks || !ptr) return undefined;
  return marks.get(ptr) ?? ([...marks.keys()].some((k) => k.startsWith(`${ptr}/`)) ? 'changed' : undefined);
}

function BoxView({ data: { box } }: NodeProps<BoxNode>) {
  const act = useContext(Acts);
  const marks = useContext(DiffContext);
  const { items, flash } = useReview();
  const { decl } = box;
  const under = itemsUnder(items, `/${decl.name}`);
  const m = worst(under);
  const edge = m ? BORDER[m] : box.focus ? 'border-link' : 'border-line-strong';
  const said = commentsUnder(useComments().all, `/${decl.name}`).length;
  const talk = said ? `, ${said} ${said === 1 ? 'comment' : 'comments'}` : '';
  const name = box.focus ? `${decl.name}${changeWords(under)}${talk}, go to its declaration` : `Centre the diagram on ${decl.name}${changeWords(under)}${talk}`;
  const tip = { onPointerEnter: (e: PointerEvent<HTMLElement>) => e.pointerType === 'mouse' && act.tip(decl.name, e.currentTarget), onPointerLeave: () => act.tip(), onFocus: (e: FocusEvent<HTMLElement>) => e.currentTarget.matches(':focus-visible') && act.tip(decl.name, e.currentTarget), onBlur: () => act.tip() };
  return (
    <div style={size(box)} className={`overflow-hidden rounded-lg border bg-surface font-mono text-[12px] text-ink-2 ${edge} ${box.focus ? 'shadow-ring' : ''} ${decl.exported ? '' : 'border-dashed'} ${act.sel === decl.name ? 'outline-2 outline-offset-2 outline-link' : ''}`}>
      <Handle type="target" position={Position.Left} id="in" isConnectable={false} style={{ top: HEAD / 2 }} />
      <button
        type="button" onClick={(e) => (e.stopPropagation(), act.pick(decl.name))} aria-label={name} aria-describedby={act.tipFor === decl.name ? act.tipId : undefined} {...tip}
        style={{ height: HEAD }} className="nodrag flex w-full cursor-pointer items-center gap-2 border-0 border-b border-line bg-soft px-2.5 text-left font-mono text-[12px] font-semibold text-ink hover:underline focus-ring"
      >
        <span>{decl.name}<span className="font-normal text-muted">{decl.generics}</span></span>
        {decl.kind !== 'other' && <span className="text-[10px] font-normal tracking-wide text-muted uppercase">{decl.kind}</span>}
        {(m || said > 0) && <span className="ml-auto flex items-center gap-2 text-[11px] text-ink-2">{said > 0 && <span className="flex items-center gap-0.5 text-link"><CommentGlyph />{said}</span>}{m && <span className="flex items-center gap-1"><Glyph m={m} />{under.length}</span>}</span>}
      </button>
      {box.rows.map((r, i) => {
        const rm = rowMark(marks, r.ptr);
        const hit = !!r.ptr && flash?.ptr === r.ptr;
        return (
          <div key={i} style={{ height: ROW }} className={`relative flex items-center gap-1.5 px-2.5 whitespace-nowrap ${hit ? 'bg-link-bg' : rm ? 'bg-warm' : ''}`}>
            {rm && <span className="absolute left-0.5"><Glyph m={rm} /></span>}
            <span className="min-w-0 truncate">{r.name} <span className="text-success">{clip(r.type)}</span></span>
            {r.chips.map((c) => <Chip key={c} name={c} />)}
            {r.edges.length > 0 && <Handle type="source" position={Position.Right} id={`r${i}`} isConnectable={false} />}
          </div>
        );
      })}
    </div>
  );
}

// A type outside this view, or named by many: pressing it centres the diagram there.
function Chip({ name }: { name: string }) {
  const act = useContext(Acts);
  const under = itemsUnder(useReview().items, `/${name}`);
  return <button type="button" onClick={() => act.focus(name)} aria-label={`Centre the diagram on ${name}${changeWords(under)}`} className={chip}>{under.length > 0 && <Glyph m={worst(under)} />}{name}</button>;
}

const nodeTypes = { box: BoxView };

/** The focused view as React Flow nodes and edges, once ELK has placed it; undefined while it lays out. */
function useFlow(view: Focused, items: Item[]) {
  const [flow, setFlow] = useState<{ nodes: BoxNode[]; edges: Edge[]; width: number; height: number }>();
  useEffect(() => {
    let live = true;
    void place(view).then(({ at, width, height }) => live && setFlow({
      width, height,
      nodes: view.boxes.map((box) => ({ id: box.decl.name, type: 'box', position: at.get(box.decl.name) ?? { x: 0, y: 0 }, data: { box }, ...size(box), ariaLabel: `${box.decl.name}${box.focus ? ', centred' : ''}${changeWords(itemsUnder(items, `/${box.decl.name}`))}` })),
      edges: view.edges.map((e) => ({ id: e.id, source: e.source, sourceHandle: `r${e.row}`, target: e.target, targetHandle: 'in', type: 'smoothstep', markerEnd: { type: MarkerType.ArrowClosed }, ariaLabel: `${e.source} uses ${e.target}` })),
    }));
    return () => void (live = false);
  }, [view, items]);
  return flow;
}

const READABLE = 0.6, INSET = 16, CONTROLS = 56, TALL = 420;

function useWidth(el: RefObject<HTMLDivElement | null>) {
  const [w, setW] = useState(0);
  useEffect(() => {
    if (!el.current) return;
    const ro = new ResizeObserver(([e]) => setW(e!.contentRect.width));
    ro.observe(el.current);
    setW(el.current.clientWidth);
    return () => ro.disconnect();
  }, [el]);
  return w;
}

// Under 640 px the frame is only as tall as what it opens on, at the zoom that fits its width, so the strip and the spec stay in view.
function frameHeight(room: number, w: number, h: number) {
  if (!room || room >= 640 || !w) return undefined;
  const k = Math.max(0.2, Math.min(1, (room - 2 * INSET) / w));
  return Math.min(TALL, Math.ceil(h * k) + 2 * INSET + CONTROLS);
}

// Fitted below READABLE the text cannot be read, so a narrow frame opens on the focused box and pans from there.
function fitting(flow: { nodes: BoxNode[]; width: number; height: number } | undefined, focus: string | undefined, room: number) {
  const fitAll = !flow || flow.width * READABLE <= room;
  const opened = flow?.nodes.find((n) => n.id === focus);
  const height = flow && (fitAll ? frameHeight(room, flow.width, flow.height) : frameHeight(room, opened?.width ?? 0, opened?.height ?? 0));
  return { fitAll, height };
}

/** What a selected box is, how many changes it has, and where to go from it. */
function Strip({ decl, items, centred, onCentre }: { decl: Decl; items: Item[]; centred: boolean; onCentre: () => void }) {
  const { show } = useReview();
  const m = worst(items);
  const btn = 'min-h-11 cursor-pointer rounded-full border border-line bg-surface px-4 text-[14px] font-semibold text-ink hover:bg-soft focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-link';
  return (
    <div className="flex flex-col gap-2 rounded-xl border border-link/40 bg-link-bg/60 p-3">
      <p className="m-0 flex flex-wrap items-center gap-2"><code className="font-mono text-[14px] font-semibold">{decl.name}</code><span className="rounded bg-soft px-1.5 text-[11px] tracking-wide text-muted uppercase">{decl.kind}</span></p>
      <p className="m-0 text-[14px] leading-snug text-ink-2">{lede(decl.doc) || (decl.doc ? 'Its description is in the spec.' : 'It has no description.')}</p>
      {m && <p className={`m-0 flex items-center gap-2 text-[13px] font-semibold ${m === 'breaking' ? 'text-danger' : 'text-warning'}`}><Glyph m={m} />{items.length} {items.length === 1 ? 'change' : 'changes'}: {items.map((i) => i.name.split(/[.›]/).at(-1)!.trim()).join(', ')}</p>}
      <div className="flex flex-wrap gap-2">
        <button type="button" onClick={() => show(`/${decl.name}`)} className={btn}>Show in spec</button>
        {!centred && <button type="button" onClick={onCentre} className={btn}>Centre here</button>}
        {decl.exported && <StripComment target={docTarget(`/${decl.name}`, lede(decl.doc) || decl.doc, 'Diagram')} />}
      </div>
    </div>
  );
}

function Caption({ id, legend, touch }: { id: string; legend: boolean; touch: boolean }) {
  return (
    <p id={id} className="m-0 text-[13px] leading-relaxed text-muted">
      An arrow leaves from the field that names a type. {touch ? 'Tap a box to see what it is; tap again to centre on it.' : 'Point at a box to see what it is; click it to centre on it.'}
      {legend && <> <span className="whitespace-nowrap"><span className="inline-flex align-middle"><Glyph m="changed" /></span> changed</span> <span className="whitespace-nowrap"><span className="inline-flex align-middle"><Glyph m="breaking" /></span> breaks a caller</span></>}
    </p>
  );
}

function Tip({ id, decl, at }: { id: string; decl: Decl; at: DOMRect }) {
  const left = Math.max(8, Math.min(at.left, innerWidth - 296));
  const flip = at.bottom + 70 > innerHeight && at.top > innerHeight - at.bottom;
  const style = flip ? { left, bottom: Math.max(8, innerHeight - at.top + 6) } : { left, top: Math.max(8, Math.min(at.bottom + 6, innerHeight - 70)) };
  return createPortal(<div id={id} role="tooltip" style={style} className="pointer-events-none fixed z-50 max-w-[280px] rounded-lg bg-ink px-2.5 py-1.5 text-[13px] leading-snug text-on-ink shadow-lift">{lede(decl.doc) || (decl.doc ? 'Its description is in the spec.' : 'No description.')}</div>, document.body);
}

/** The contract's types around one in focus: what it uses and what uses it, each arrow leaving from the field that holds the reference.
 * A box shows what it is on hover, focus or a first tap; pressing it centres the diagram there, and pressing the centred box finds it in the spec. */
export function ClassDiagram({ decls }: { decls: Decl[] }) {
  const review = useReview();
  const [focus, setFocus] = useState(() => defaultFocus(decls));
  const [sel, setSel] = useState<string>();
  const [tip, setTip] = useState<{ name: string; at: DOMRect }>();
  const pointer = useRef<string>(undefined);
  useEffect(() => setFocus((f) => (f && decls.some((d) => d.name === f) ? f : defaultFocus(decls))), [decls]);
  // A fixed tooltip would drift from its box as anything scrolls, so a scroll hides it.
  useEffect(() => {
    if (!tip) return;
    const hide = () => setTip(undefined);
    addEventListener('scroll', hide, true);
    return () => removeEventListener('scroll', hide, true);
  }, [tip]);
  const section = useRef<HTMLElement>(null);
  useEffect(() => {
    const n = aimAt(decls, review.aim?.ptr);
    if (n) (setFocus(n), setSel(undefined));
    if (n && review.aim?.look) section.current?.scrollIntoView?.({ block: 'start' });
  }, [review.aim]);
  const view = useMemo(() => (focus ? focusView(decls, focus) : { boxes: [], edges: [] }), [decls, focus]);
  const flow = useFlow(view, review.items);
  const tipId = useId(), note = useId();
  // A touch selects first and centres on a second tap; a mouse or key centres at once. A press on the centred box goes to the spec.
  const acts = useMemo<Act>(() => ({
    sel, tipId, tipFor: tip?.name,
    focus: (n) => (setFocus(n), setSel(undefined)),
    tip: (n, el) => setTip(n && el ? { name: n, at: el.getBoundingClientRect() } : undefined),
    pick: (n) => {
      const touch = !!pointer.current && pointer.current !== 'mouse';
      pointer.current = undefined;
      if (touch && sel !== n) return setSel(n);
      if (n !== focus) return (setFocus(n), setSel(touch ? n : undefined));
      review.show(`/${n}`);
    },
  }), [sel, focus, tip, tipId, review.show]);
  const frame = useRef<HTMLDivElement>(null);
  // On a touch screen one finger scrolls the page past the diagram; it moves by taps (select, centre) and the zoom buttons instead.
  const touch = useMedia('(pointer: coarse)');
  const room = useWidth(frame);
  const { fitAll, height } = fitting(flow, focus, room);
  // With nothing tapped, the strip is the centred box's, so a mouse reaches its actions too.
  const selected = decls.find((d) => d.name === (sel ?? focus));
  if (!view.boxes.length) return null;
  return (
    <section ref={section} aria-label="Class diagram" aria-describedby={note} className="flex min-w-0 scroll-mt-16 flex-col gap-3 rounded-[18px] border border-line bg-surface p-4">
      <h2 className="m-0 text-[17px] font-semibold">Class diagram <span className="font-normal text-muted">around {focus}</span></h2>
      <div ref={frame} onPointerDownCapture={(e) => void (pointer.current = e.pointerType)} style={height ? { height } : undefined} className={`class-diagram h-[420px] overflow-hidden rounded-xl border border-line sm:h-[520px] ${touch ? 'diagram-touch' : ''}`}>
        <Acts value={acts}>
          {flow && (
            <ReactFlow
              key={`${focus}:${height ?? ''}`} nodes={flow.nodes} edges={flow.edges} nodeTypes={nodeTypes} fitView minZoom={0.2}
              fitViewOptions={fitAll ? { padding: { x: `${INSET}px`, top: `${INSET}px`, bottom: `${CONTROLS}px` } } : { nodes: [{ id: focus! }], maxZoom: 1, padding: { x: `${INSET}px`, top: `${INSET}px`, bottom: `${CONTROLS}px` } }}
              nodesDraggable={false} nodesConnectable={false} panOnDrag={!touch} zoomOnScroll={false} preventScrolling={false} onNodeClick={(_, n) => acts.pick(n.id)}
              proOptions={{ hideAttribution: true }}
            >
              <Background gap={20} size={1} />
              <Controls showInteractive={false} orientation="horizontal" />
              {room >= 640 && <MiniMap pannable zoomable ariaLabel="Overview of the class diagram" />}
            </ReactFlow>
          )}
        </Acts>
      </div>
      <Caption id={note} legend={review.items.length > 0} touch={touch} />
      <div aria-live="polite">{selected && <Strip decl={selected} items={itemsUnder(review.items, `/${selected.name}`)} centred={selected.name === focus} onCentre={() => setFocus(selected.name)} />}</div>
      {tip && decls.some((d) => d.name === tip.name) && <Tip id={tipId} decl={decls.find((d) => d.name === tip.name)!} at={tip.at} />}
    </section>
  );
}
