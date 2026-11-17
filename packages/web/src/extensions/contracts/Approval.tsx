import { useState } from 'react';
import { decideApproval, type Approval, type Verdict } from '../../api.ts';
import { CheckResult } from '../../CheckResult.tsx';
import { BreakRows, ChangeRows, label, listedBreaks, since, type Listed } from './Sections.tsx';
import type { ContractSpec } from './spec.ts';

/** One break the contracts app's code checker found between the approved version and this one. */
export type Break = { id?: string; text: string; operation?: string; path?: string };

// A TypeScript break's path is `/Export`; the export's name is how a person knows it.
const where = (b: Break) => `${b.operation ?? ''} ${(b.path ?? '').replace(/^\/(?=[A-Za-z_$][\w$]*$)/, '')}`.trim() || b.text;
const esc = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
// The names prose would use for a break's place: its path, and its last segment when that is an identifier (an export, a property).
function names(b: Break): string[] {
  const path = b.path ?? '';
  const last = path.split('/').filter(Boolean).at(-1) ?? '';
  return [...(path.length > 1 ? [path] : []), ...(/^[A-Za-z_$][\w$]*$/.test(last) ? [last] : [])];
}

/** The places the checker found a break that no listed break names, as a whole word in its what or migration. A text match, not a proof. */
export function unnamedBreaks(listed: Listed[], found: Break[]): string[] {
  const text = listed.map((b) => `${b.what}\n${b.migration ?? ''}`).join('\n');
  const said = (n: string) => new RegExp(`(^|[^\\w$])${esc(n)}($|[^\\w$])`).test(text);
  return [...new Set(found.filter((b) => !names(b).some(said)).map(where))];
}

const NOUN: Record<ContractSpec['kind'], [string, string, string]> = { typescript: ['export', 'exports', 'TypeScript'], rest: ['route', 'routes', 'REST'], 'json-schema': ['place', 'places', 'JSON Schema'] };
const SIDE: Record<string, string> = { 'produced-breaks': 'what core produces no longer fits old consumers', 'provided-breaks': 'what extensions provide no longer fits', 'export-removed': 'removed' };
// Text the checker cut ends in an ellipsis; one stored before that was cut bare at 300 characters after its `export "X": side: ` prefix.
const wasCut = (b: Break) => b.text.endsWith('…') || (!!b.id && b.id in SIDE && b.id !== 'export-removed' && b.text.replace(/^export "[^"]*": [^:]*: /, '').length === 300);

/** The checker's own output, one block per place, monospaced and wrapped, never cut by the page. */
function CheckerOutput({ found }: { found: Break[] }) {
  const groups = Map.groupBy(found, where);
  return (
    <details className="text-[13px]">
      <summary className="flex min-h-11 cursor-pointer items-center text-link">Checker output ({found.length})</summary>
      <ul className="m-0 flex list-none flex-col gap-2 p-0">
        {[...groups].map(([at, bs]) => (
          <li key={at} className="flex min-w-0 flex-col gap-1">
            <span className="font-mono text-[12px] font-semibold text-ink [overflow-wrap:anywhere]">{at}</span>
            {bs.map((b, i) => (
              <div key={i} className="min-w-0 rounded-lg border border-line bg-canvas p-2.5">
                {b.id && <span className="block text-[11.5px] text-muted">{SIDE[b.id] ?? b.id.replace(/-/g, ' ')}</span>}
                <pre className="m-0 font-mono text-[12px] leading-relaxed whitespace-pre-wrap text-ink-2 [overflow-wrap:anywhere]">{b.text}</pre>
                {wasCut(b) && <span className="text-[11.5px] text-muted">(cut by the checker)</span>}
              </div>
            ))}
          </li>
        ))}
      </ul>
    </details>
  );
}

// Whether the checker and the spec agree on what breaks; a break the spec does not name is the thing to read before approving.
function Checker({ spec, found }: { spec: ContractSpec; found: Break[] }) {
  const unnamed = unnamedBreaks(listedBreaks(spec), found);
  const [one, many] = NOUN[spec.kind];
  const n = new Set(found.map(where)).size;
  const line = !found.length ? 'The code checker found no break against the approved version.'
    : unnamed.length ? `The code checker found breaks in ${unnamed.length} ${unnamed.length === 1 ? one : many} not named in the spec's breaks: ${unnamed.join(', ')}`
    : `Every ${one} the code checker found breaking (${n}) is named in the spec's breaks.`;
  return (
    <div className="flex flex-col">
      <p className={`m-0 flex gap-2 text-[13px] [overflow-wrap:anywhere] ${unnamed.length ? 'rounded-xl bg-warning-bg px-3 py-2 text-warning' : 'text-ink-2'}`}>
        <span aria-hidden="true">{unnamed.length ? '⚠' : '✓'}</span><span>{line}</span>
      </p>
      {found.length > 0 && <CheckerOutput found={found} />}
    </div>
  );
}

const btn = 'min-h-11 cursor-pointer rounded-full px-4 text-[14px] font-semibold disabled:cursor-not-allowed disabled:opacity-50 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-link lg:min-h-9';
const prim = `${btn} border-0 bg-ink text-on-ink hover:bg-ink-2`;
const sec = `${btn} border border-line bg-surface text-ink-2 hover:text-ink`;

type Decide = (decision: 'approve' | 'reject', reason?: string) => Promise<void>;

/** Approve, or ask for changes with a reason the agent reads. */
function Actions({ decide, busy }: { decide: Decide; busy?: 'approve' | 'reject' }) {
  const [asking, setAsking] = useState(false);
  const [reason, setReason] = useState('');
  if (!asking) return (
    <div className="flex flex-wrap gap-2">
      <button type="button" disabled={!!busy} onClick={() => void decide('approve')} className={prim}>{busy === 'approve' ? 'Approving…' : 'Approve'}</button>
      <button type="button" disabled={!!busy} onClick={() => setAsking(true)} className={sec}>Ask for changes</button>
    </div>
  );
  return (
    <form className="flex flex-col gap-2" onSubmit={(e) => (e.preventDefault(), void decide('reject', reason.trim()))}>
      <label className="flex flex-col gap-1 text-[13px] text-ink-2">
        What should change?
        <textarea required value={reason} onChange={(e) => setReason(e.target.value)} rows={3} className="focus-ring rounded-lg border border-line bg-surface px-3 py-2 text-sm text-ink" />
      </label>
      <div className="flex flex-wrap gap-2">
        <button type="submit" disabled={!!busy || !reason.trim()} className={prim}>{busy === 'reject' ? 'Sending…' : 'Send'}</button>
        <button type="button" disabled={!!busy} onClick={() => setAsking(false)} className={sec}>Cancel</button>
      </div>
    </form>
  );
}

export type Pending = { spec: ContractSpec; verdict?: Verdict | null; found?: Break[] };

/** What the person decides on, in the order they read it: the checks, what breaks, what changed, then the actions. */
export function Decision({ sessionKey, proposal, pending, decided }: { sessionKey: string; proposal: Approval; pending: Pending; decided: (id: string) => void }) {
  const [busy, setBusy] = useState<'approve' | 'reject'>();
  const [error, setError] = useState('');
  const { spec, verdict, found } = pending;
  const decide: Decide = async (decision, reason) => {
    setBusy(decision);
    setError('');
    try {
      if (await decideApproval(sessionKey, proposal.id, decision, reason)) decided(proposal.id);
      else setError('This version was already decided, in another tab or by a message you sent. The page shows where it stands now.');
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  };
  const listed = listedBreaks(spec).length, changes = spec.changes?.length ?? 0;
  return (
    <>
      <div className="flex flex-col px-4 pb-1">
        <p className="m-0 -mt-2 text-[13px] text-muted">{NOUN[spec.kind][2]} contract · changes measured against {since(spec)}</p>
        {verdict ? <CheckResult verdict={verdict} open={false} summary="Automatic checks passed" /> : <p className="m-0 flex min-h-11 items-center text-[13px] text-success">Automatic checks passed</p>}
      </div>
      <section aria-label="Breaking changes" className="flex flex-col border-t border-line">
        <h4 className={`m-0 px-4 pt-3 pb-1 font-sans ${label}`}>{listed ? `Breaking changes · ${listed} in the spec` : 'The spec lists no breaks'}</h4>
        <BreakRows spec={spec} />
        {found && <div className="px-4 py-2"><Checker spec={spec} found={found} /></div>}
      </section>
      {changes > 0 && <section aria-label="What changed" className="border-t border-line">
        <h4 className={`m-0 px-4 pt-3 pb-1 font-sans ${label}`}>Changed from {since(spec)} · {changes}</h4>
        <ChangeRows spec={spec} />
      </section>}
      <div className="flex flex-col gap-2 border-t border-line px-4 py-3">
        {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
        <Actions decide={decide} busy={busy} />
      </div>
    </>
  );
}
