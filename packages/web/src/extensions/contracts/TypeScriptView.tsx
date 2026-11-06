import { lazy, Suspense, useEffect, useState } from 'react';
import type { Verdict } from '../../api.ts';
import { unplaced } from './Info.tsx';
import { Changes, Compatibility, decisionCount, download, Header, Purpose, type ViewProps } from './RestView.tsx';
import type { ContractSpec, Model } from './spec.ts';

// The TypeScript compiler and the diagram layout load only when a full TypeScript contract opens.
const CodeContract = lazy(() => import('./CodeContract.tsx').then((m) => ({ default: m.CodeContract })));

// Undefined while the parser loads, null when it fails; both show the source as it stands.
export function useModel(src: string) {
  const [model, setModel] = useState<Model | null>();
  useEffect(() => {
    if (!src) return;
    let live = true;
    import('./typescript-model.ts').then((m) => m.declarations(src)).then((m) => live && setModel(m), () => live && setModel(null));
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

// The ledger card: the declarations' size and purpose; the source and its decisions live on the full page.
function CardView({ spec, src, verdict }: { spec: ContractSpec; src: string; verdict?: Verdict | null }) {
  return (
    <div className="@container flex min-w-0 flex-col gap-4">
      <Header spec={spec} count={count(src)} full={false} file={file(src)} verdict={verdict} />
      <Purpose spec={spec} />
      <p className="m-0 text-[13px] text-muted">{decisionCount(spec)}</p>
    </div>
  );
}

export function TypeScriptView({ spec, full = false, verdict }: ViewProps) {
  const src = spec.document as string;
  const model = useModel(full ? src : '');
  if (!full) return <CardView spec={spec} src={src} verdict={verdict} />;
  const plain = <section aria-label="Declarations" className="min-w-0 rounded-[18px] border border-line bg-surface p-4"><pre className={code}>{src}</pre></section>;
  // Until the declarations are parsed, every decision sits on the title.
  const drawn = new Set(model ? model.decls.map((d) => `/${d.name}`) : []);
  return (
    <div className="@container flex min-w-0 flex-col gap-4">
      <Header spec={spec} count={count(src)} full file={file(src)} verdict={verdict} info={unplaced(spec.choices, drawn)} />
      <Changes spec={spec} />
      <Compatibility spec={spec} />
      <Purpose spec={spec} />
      {model ? <Suspense fallback={plain}><CodeContract model={model} source={src} choices={spec.choices} /></Suspense> : plain}
    </div>
  );
}
