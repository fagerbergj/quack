import { useState } from 'react';
import { Changes, Compatibility, Decisions, decisionCount, download, Header, Purpose, WhyPanel } from './RestView.tsx';
import type { Choice, ContractSpec } from './spec.ts';

const count = (src: string) => {
  const n = src.match(/^export /gm)?.length ?? 0;
  return `${n} ${n === 1 ? 'export' : 'exports'}`;
};
const file = (src: string) => ({ label: 'contract.d.ts', save: () => download(src.endsWith('\n') ? src : `${src}\n`, 'text/plain', 'contract.d.ts') });
const code = 'm-0 overflow-x-auto rounded-xl border border-line bg-canvas p-3 font-mono text-xs leading-relaxed whitespace-pre text-ink-2';

// The ledger card: the declarations' size and purpose; the source and its decisions live on the full page.
function CardView({ spec, src }: { spec: ContractSpec; src: string }) {
  return (
    <div className="@container flex min-w-0 flex-col gap-4">
      <Header spec={spec} count={count(src)} full={false} file={file(src)} />
      <Purpose spec={spec} />
      <p className="m-0 text-[13px] text-muted">{decisionCount(spec)}</p>
    </div>
  );
}

export function TypeScriptView({ spec, full = false }: { spec: ContractSpec; full?: boolean }) {
  const [why, setWhy] = useState<Choice>();
  const src = spec.document as string;
  if (!full) return <CardView spec={spec} src={src} />;
  return (
    <div className="@container flex min-w-0 flex-col gap-4">
      <Header spec={spec} count={count(src)} full file={file(src)} />
      <Changes spec={spec} onWhy={setWhy} />
      <Compatibility spec={spec} />
      <Purpose spec={spec} />
      <div className="grid grid-cols-1 items-start gap-4 @[1100px]:grid-cols-[minmax(0,1fr)_360px]">
        <section aria-label="Declarations" className="min-w-0 rounded-[18px] border border-line bg-surface p-4"><pre className={code}>{src}</pre></section>
        <div className="flex min-w-0 flex-col gap-4">
          {why && <WhyPanel choice={why} onClose={() => setWhy(undefined)} />}
          <Decisions spec={spec} onWhy={setWhy} />
        </div>
      </div>
    </div>
  );
}
