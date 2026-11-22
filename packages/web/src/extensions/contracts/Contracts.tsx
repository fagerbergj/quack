import { useState } from 'react';
import { Modal, useAction } from '../../Housekeeping.tsx';
import type { Row } from '../../sessions.ts';
import { useLoad } from '../../useLoad.ts';
import { VersionMark } from '../../StatusIcon.tsx';
import { contractRecords, contractRows, startContract, type Contract, type ContractRecord } from './status.ts';

// Approved contracts past this many wait behind a button.
const SHOWN = 10;

const NO_RECORDS: ReadonlyMap<string, ContractRecord> = new Map();

function NewContract({ onClose }: { onClose: () => void }) {
  const [text, setText] = useState('');
  const { busy, error, go } = useAction(() => startContract(text).then((key) => void (location.hash = `#/s/${encodeURIComponent(key)}`)), onClose);
  return (
    <Modal title="New contract" onClose={onClose} onSubmit={() => text.trim() && go()} action="Start" busy={busy} error={error}>
      <label className="flex flex-col gap-1 text-[13px] text-muted">
        What should it do, and for whom?
        <textarea autoFocus value={text} onChange={(e) => setText(e.target.value)} className="field-grow focus-ring min-h-[3lh] max-h-[12lh] rounded-xl border border-line bg-surface px-3 py-2 text-[15px] text-ink" />
      </label>
    </Modal>
  );
}

// The kind tag holds its column even when a contract has no record yet, so titles line up.
function Item({ c }: { c: Contract }) {
  return (
    <li>
      <a href={`#/s/${encodeURIComponent(c.key)}`} className="flex min-h-13 items-center gap-3 px-3 py-2 text-ink no-underline hover:bg-soft sm:px-4">
        <span title={c.kind?.name} className="w-10 flex-none rounded-md bg-soft py-0.5 text-center font-mono text-[10px] font-bold text-muted empty:bg-transparent">{c.kind?.tag}</span>
        <span className="line-clamp-2 min-w-0 flex-1 text-[15px] font-semibold [overflow-wrap:anywhere]">{c.name}</span>
        <VersionMark version={c.version} mark={c.mark} label={c.status} />
      </a>
    </li>
  );
}

// The group's name says the status once, so its rows carry only a version and a mark.
function Group({ label, items, limit }: { label: string; items: Contract[]; limit?: number }) {
  const [all, setAll] = useState(false);
  if (!items.length) return null;
  const shown = all || !limit ? items : items.slice(0, limit);
  return (
    <section aria-label={label} className="mb-5">
      <h2 className="m-0 mb-1.5 flex items-baseline gap-2 px-1 font-sans text-xs font-semibold tracking-wider text-muted uppercase">{label}<span className="font-normal tabular-nums">{items.length}</span></h2>
      <ul className="m-0 flex list-none flex-col divide-y divide-line overflow-hidden rounded-[22px] border border-line bg-surface p-0 shadow-card">
        {shown.map((c) => <Item key={c.key} c={c} />)}
        {shown.length < items.length && (
          <li><button type="button" onClick={() => setAll(true)} className="flex min-h-11 w-full cursor-pointer items-center justify-center border-0 bg-transparent text-[13px] text-link hover:bg-soft">{`${items.length - shown.length} more ${label.toLowerCase()}`}</button></li>
        )}
      </ul>
    </section>
  );
}

export function Contracts({ rows }: { rows: Row[] | undefined }) {
  const [adding, setAdding] = useState(false);
  // Read again when a contract session starts, ends or changes state, which is when a version can move.
  const shape = (rows ?? []).filter((r) => r.key.startsWith('contracts:')).map((r) => `${r.key}:${r.state}:${r.pendingAsks}`).join(',');
  const { data: records } = useLoad(contractRecords, [shape], NO_RECORDS);
  const list = contractRows(rows ?? [], records);
  return (
    <div className="mx-auto w-full max-w-[960px] px-4 pt-4 pb-10 sm:px-6 lg:pt-8">
      <div className="mb-4 flex items-center gap-3">
        <h1 className="m-0 hidden flex-1 text-[22px] font-bold tracking-[-0.02em] lg:block">Contracts</h1>
        <button type="button" aria-label="New contract" title="New contract" onClick={() => setAdding(true)} className="ml-auto grid h-11 w-11 flex-none cursor-pointer place-items-center rounded-full border-0 bg-ink text-on-ink hover:bg-ink-2 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-link">
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" aria-hidden="true"><path d="M12 5v14M5 12h14" /></svg>
        </button>
      </div>
      {rows && !list.length && <p className="text-sm text-muted">A contract is the agreed shape of an interface, such as a REST endpoint, that you approve before anyone builds against it.</p>}
      <Group label="In progress" items={list.filter((c) => !c.approved)} />
      <Group label="Approved" items={list.filter((c) => c.approved)} limit={SHOWN} />
      {adding && <NewContract onClose={() => setAdding(false)} />}
    </div>
  );
}
