import { Background, Controls, Handle, MarkerType, MiniMap, Position, ReactFlow, type Edge, type Node, type NodeProps } from '@xyflow/react';
import '@xyflow/react/dist/style.css';
import ELK, { type ElkNode } from 'elkjs/lib/elk.bundled.js';
import { createContext, useContext, useEffect, useId, useMemo, useRef, useState, type RefObject } from 'react';
import { defaultFocus, focusView, type Box, type Focused } from './class-model.ts';
import type { Decl } from './spec.ts';

// Monospace at 12px; boxes are sized from character counts, since ELK needs sizes before anything is drawn.
const CH = 7.25, ROW = 22, HEAD = 32, PAD = 10, MAX_CHARS = 48;
const clip = (s: string) => (s.length > MAX_CHARS ? `${s.slice(0, MAX_CHARS - 1)}…` : s);
const rowText = (r: Box['rows'][number]) => `${r.name} ${clip(r.type)}`;
const size = (b: Box) => ({
  width: Math.ceil(Math.max(b.decl.name.length + (b.decl.generics?.length ?? 0) + b.decl.kind.length + 4, ...b.rows.map((r) => rowText(r).length + r.chips.join('  ').length + 2 * r.chips.length)) * CH + 2 * PAD + 12),
  height: HEAD + b.rows.length * ROW + 8,
});

const elk = new ELK();

/** Places the boxes left to right with ELK's layered algorithm: one port per field that holds an arrow, in field order, so arrows leave from their field. */
async function place(view: Focused): Promise<{ at: Map<string, { x: number; y: number }>; width: number }> {
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
  return { at: new Map((laid.children ?? []).map((c) => [c.id, { x: c.x ?? 0, y: c.y ?? 0 }])), width: laid.width ?? 0 };
}

type Act = { focus: (name: string) => void; jump: (name: string) => void };
const Acts = createContext<Act>({ focus: () => {}, jump: () => {} });
type BoxNode = Node<{ box: Box }, 'box'>;

const chip = 'nodrag cursor-pointer rounded border-0 bg-link-bg px-1 font-mono text-[11px] text-link hover:underline focus-visible:shadow-ring';

function BoxView({ data: { box } }: NodeProps<BoxNode>) {
  const act = useContext(Acts);
  const { decl } = box;
  return (
    <div style={size(box)} className={`overflow-hidden rounded-lg border bg-surface font-mono text-[12px] text-ink-2 ${box.focus ? 'border-link shadow-ring' : 'border-line-strong'} ${decl.exported ? '' : 'border-dashed'}`}>
      <Handle type="target" position={Position.Left} id="in" isConnectable={false} style={{ top: HEAD / 2 }} />
      <button
        type="button" onClick={() => (box.focus ? act.jump(decl.name) : act.focus(decl.name))}
        aria-label={box.focus ? `${decl.name}, go to its declaration` : `Centre the diagram on ${decl.name}`}
        style={{ height: HEAD }} className="nodrag flex w-full cursor-pointer items-center gap-2 border-0 border-b border-line bg-soft px-2.5 text-left font-mono text-[12px] font-semibold text-ink hover:underline focus-visible:shadow-ring"
      >
        <span>{decl.name}<span className="font-normal text-muted">{decl.generics}</span></span>
        {decl.kind !== 'other' && <span className="text-[10px] font-normal tracking-wide text-muted uppercase">{decl.kind}</span>}
      </button>
      {box.rows.map((r, i) => (
        <div key={i} style={{ height: ROW }} className="relative flex items-center gap-1.5 px-2.5 whitespace-nowrap">
          <span className="min-w-0 truncate">{r.name} <span className="text-success">{clip(r.type)}</span></span>
          {r.chips.map((c) => <button key={c} type="button" onClick={() => act.focus(c)} aria-label={`Centre the diagram on ${c}`} className={chip}>{c}</button>)}
          {r.edges.length > 0 && <Handle type="source" position={Position.Right} id={`r${i}`} isConnectable={false} />}
        </div>
      ))}
    </div>
  );
}

const nodeTypes = { box: BoxView };

/** The focused view as React Flow nodes and edges, once ELK has placed it; undefined while it lays out. */
function useFlow(view: Focused) {
  const [flow, setFlow] = useState<{ nodes: BoxNode[]; edges: Edge[]; width: number }>();
  useEffect(() => {
    let live = true;
    void place(view).then(({ at, width }) => live && setFlow({
      width,
      nodes: view.boxes.map((box) => ({ id: box.decl.name, type: 'box', position: at.get(box.decl.name) ?? { x: 0, y: 0 }, data: { box }, ...size(box), ariaLabel: `${box.decl.name}${box.focus ? ', centred' : ''}` })),
      edges: view.edges.map((e) => ({ id: e.id, source: e.source, sourceHandle: `r${e.row}`, target: e.target, targetHandle: 'in', type: 'smoothstep', markerEnd: { type: MarkerType.ArrowClosed }, ariaLabel: `${e.source} uses ${e.target}` })),
    }));
    return () => void (live = false);
  }, [view]);
  return flow;
}

const READABLE = 0.6;

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

/** The contract's types around one in focus: what it uses and what uses it, each arrow leaving from the field that holds the reference.
 * A box or chip re-centres the diagram; the declarations below list every type, so they are its text alternative. */
export function ClassDiagram({ decls, onJump }: { decls: Decl[]; onJump: (name: string) => void }) {
  const [focus, setFocus] = useState(() => defaultFocus(decls));
  useEffect(() => setFocus((f) => (f && decls.some((d) => d.name === f) ? f : defaultFocus(decls))), [decls]);
  const view = useMemo(() => (focus ? focusView(decls, focus) : { boxes: [], edges: [] }), [decls, focus]);
  const flow = useFlow(view);
  const acts = useMemo(() => ({ focus: setFocus, jump: onJump }), [onJump]);
  const note = useId();
  const frame = useRef<HTMLDivElement>(null);
  const room = useWidth(frame);
  // Fitted below READABLE the text cannot be read, so a narrow frame opens on the focused box and pans from there.
  const fitAll = !flow || flow.width * READABLE <= room;
  if (!view.boxes.length) return null;
  return (
    <section aria-label="Class diagram" aria-describedby={note} className="min-w-0 rounded-[18px] border border-line bg-surface p-4">
      <h2 className="m-0 mb-3 text-[15px] font-semibold">Class diagram <span className="font-normal text-muted">around {focus}</span></h2>
      <div ref={frame} className="class-diagram h-[420px] overflow-hidden rounded-xl border border-line sm:h-[520px]">
        <Acts value={acts}>
          {flow && (
            <ReactFlow
              key={focus} nodes={flow.nodes} edges={flow.edges} nodeTypes={nodeTypes} fitView fitViewOptions={fitAll ? undefined : { nodes: [{ id: focus! }], maxZoom: 1 }} minZoom={0.2}
              nodesDraggable={false} nodesConnectable={false} zoomOnScroll={false} preventScrolling={false} onNodeClick={(_, n) => n.id !== focus && setFocus(n.id)}
              proOptions={{ hideAttribution: true }}
            >
              <Background gap={20} size={1} />
              <Controls showInteractive={false} />
              {room >= 640 && <MiniMap pannable zoomable ariaLabel="Overview of the class diagram" />}
            </ReactFlow>
          )}
        </Acts>
      </div>
      <p id={note} className="m-0 mt-2 text-xs text-muted">{focus} with the types it uses and that use it. An arrow leaves from the field that names a type; a chip names one outside this view or used by many. Select a box or a chip to centre on it; the declarations below list every type.</p>
    </section>
  );
}
