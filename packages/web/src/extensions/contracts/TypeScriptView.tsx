import { lazy, Suspense, useEffect, useState } from 'react';
import { unplaced } from './Info.tsx';
import { download, Header, type ViewProps } from './RestView.tsx';
import { Changes, MoreAbout, Purpose } from './Review.tsx';
import type { Model } from './spec.ts';

// The parser and the diagram layout load only when a TypeScript contract opens, side by side rather than one after the other.
const loadCode = () => import('./CodeContract.tsx');
const CodeContract = lazy(() => loadCode().then((m) => ({ default: m.CodeContract })));

// One parse per source: the view and the comparison read the same versions. Bounded, since a session can hold dozens.
const parsed = new Map<string, Model>();
function remember(src: string, m: Model) {
  if (parsed.size >= 8) parsed.delete(parsed.keys().next().value!);
  parsed.set(src, m);
  return m;
}

// Undefined while the parser loads, null when it fails; both show the source as it stands.
export function useModel(src: string) {
  const [model, setModel] = useState<Model | null | undefined>(() => parsed.get(src));
  useEffect(() => {
    if (!src) return;
    let live = true;
    import('./typescript-model.ts').then((m) => parsed.get(src) ?? remember(src, m.declarations(src))).then((m) => live && setModel(m), () => live && setModel(null));
    return () => void (live = false);
  }, [src]);
  return model;
}

const count = (src: string) => {
  const n = src.match(/^export /gm)?.length ?? 0;
  return `${n} ${n === 1 ? 'export' : 'exports'}`;
};
const file = (src: string) => ({ label: 'contract.d.ts', save: () => download(src.endsWith('\n') ? src : `${src}\n`, 'text/plain', 'contract.d.ts') });
const code = 'm-0 overflow-x-auto rounded-xl border border-line bg-canvas p-3 font-mono text-xs leading-relaxed whitespace-pre text-ink-2';

export function TypeScriptView({ spec, verdict }: ViewProps) {
  const src = spec.document as string;
  const model = useModel(src);
  useEffect(() => void loadCode(), []);
  const plain = <section aria-label="Declarations" className="min-w-0 rounded-[18px] border border-line bg-surface p-4"><pre className={code}>{src}</pre></section>;
  const changes = <Changes spec={spec} />;
  // Until the declarations are parsed, every decision sits on the title.
  const drawn = new Set(model ? model.decls.map((d) => `/${d.name}`) : []);
  const source = { ptr: '#source', title: 'Source, contract.d.ts', body: <pre className={code}>{src}</pre> };
  return (
    <div className="@container flex min-w-0 flex-col gap-6">
      <Header spec={spec} count={count(src)} file={file(src)} verdict={verdict} info={unplaced(spec.choices, drawn)} />
      <Purpose spec={spec} />
      {model ? <Suspense fallback={<>{changes}{plain}</>}><CodeContract model={model} choices={spec.choices} changes={changes} /></Suspense> : <>{changes}{plain}</>}
      <MoreAbout spec={spec} extra={[source]} />
    </div>
  );
}
