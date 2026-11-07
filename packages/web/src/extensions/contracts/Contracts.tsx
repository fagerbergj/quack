import { useState } from 'react';
import { startContract } from '../../api.ts';
import { Modal, useAction } from '../../Housekeeping.tsx';
import type { Row } from '../../sessions.ts';
import { contractRows, type Tone } from './status.ts';

const TONE: Record<Tone, string> = { warn: 'bg-warning-bg text-warning', plain: 'bg-soft text-ink-2', bad: 'bg-danger/10 text-danger' };

function NewContract({ onClose }: { onClose: () => void }) {
  const [text, setText] = useState('');
  const { busy, error, go } = useAction(() => startContract(text).then((key) => void (location.hash = `#/s/${encodeURIComponent(key)}`)), onClose);
  return (
    <Modal title="New contract" onClose={onClose} onSubmit={() => text.trim() && go()} action="Start" busy={busy} error={error}>
      <label className="flex flex-col gap-1 text-[13px] text-muted">
        What should it do, and for whom?
        <textarea autoFocus rows={3} value={text} onChange={(e) => setText(e.target.value)} className="rounded-xl border border-line bg-surface px-3 py-2 text-[15px] text-ink outline-none focus:shadow-ring" />
      </label>
    </Modal>
  );
}

export function Contracts({ rows }: { rows: Row[] | undefined }) {
  const [adding, setAdding] = useState(false);
  const list = contractRows(rows ?? []);
  return (
    <div className="mx-auto w-full max-w-[960px] px-4 pt-4 pb-10 sm:px-6 lg:pt-8">
      <div className="mb-4 flex items-center gap-3">
        <h1 className="m-0 hidden flex-1 text-[22px] font-bold tracking-[-0.02em] lg:block">Contracts</h1>
        <button type="button" onClick={() => setAdding(true)} className="ml-auto min-h-11 cursor-pointer rounded-full border-0 bg-ink px-4 text-[14px] font-semibold text-on-ink hover:bg-ink-2 lg:min-h-9">New contract</button>
      </div>
      {rows && !list.length && <p className="text-sm text-muted">A contract is the agreed shape of an interface, such as a REST endpoint, that you approve before anyone builds against it.</p>}
      <ul className="m-0 flex list-none flex-col divide-y divide-line overflow-hidden rounded-[22px] border border-line bg-surface p-0 shadow-card empty:hidden">
        {list.map((c) => (
          <li key={c.key}>
            <a href={`#/s/${encodeURIComponent(c.key)}`} className="flex min-h-16 items-center gap-3 px-3 py-2 text-ink no-underline hover:bg-soft sm:px-4">
              <span className="min-w-0 flex-1 truncate text-[15px] font-semibold">{c.name}</span>
              <span className={`flex-none rounded-full px-2.5 py-0.5 text-xs font-medium ${TONE[c.tone]}`}>{c.status}</span>
            </a>
          </li>
        ))}
      </ul>
      {adding && <NewContract onClose={() => setAdding(false)} />}
    </div>
  );
}
