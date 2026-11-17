import { useId, useState } from 'react';
import { decideApproval, type Approval, type Verdict } from '../../api.ts';
import { CheckResult } from '../../CheckResult.tsx';
import { Inline } from './Info.tsx';
import { Glyph, Sheet } from './Review.tsx';
import { lede, type Item } from './review.ts';
import { label } from './Sections.tsx';
import { listedBreaks, type ContractSpec, type Listed } from './spec.ts';

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

const btn = 'min-h-11 cursor-pointer rounded-full px-4 text-[14px] font-semibold disabled:cursor-not-allowed disabled:opacity-50 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-link';
export const prim = `${btn} border-0 bg-ink text-on-ink hover:bg-ink-2`;
export const sec = `${btn} border border-line bg-surface text-ink hover:bg-soft`;

type Decide = (decision: 'approve' | 'reject', reason?: string) => Promise<boolean>;

/** Decides the proposal: true once decided here, else false with the reason in `error`. */
export function useDecide(sessionKey: string, proposal: Approval | undefined, decided: (id: string) => void) {
  const [busy, setBusy] = useState<'approve' | 'reject'>();
  const [error, setError] = useState('');
  const decide: Decide = async (decision, reason) => {
    if (!proposal) return false;
    setBusy(decision);
    setError('');
    try {
      if (await decideApproval(sessionKey, proposal.id, decision, reason)) return (decided(proposal.id), true);
      setError('This version was already decided, in another tab or by a message you sent. The page shows where it stands now.');
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
    return false;
  };
  return { busy, error, decide };
}

/** What the person decides on: the newest version, its changes against the approved one, and the checker's breaks. */
export type Pending = { spec: ContractSpec; verdict?: Verdict | null; found?: Break[]; items: Item[]; base?: string; name: string };
type Deciding = ReturnType<typeof useDecide> & { onClose: () => void };

const h3 = `m-0 font-sans ${label}`;

// The spec's breaks, each by its first sentence and its callers, with the code checker's own findings beside them.
function BreakLines({ spec, found }: { spec: ContractSpec; found?: Break[] }) {
  const listed = listedBreaks(spec);
  return (
    <section className="flex flex-col gap-2">
      <h3 className={h3}>{listed.length ? `What could break · ${listed.length}` : 'The spec lists no breaks'}</h3>
      {listed.length > 0 && <ul className="m-0 flex list-none flex-col gap-2 p-0">{listed.map((b, i) => (
        <li key={i} className="flex items-baseline gap-2.5 [overflow-wrap:anywhere]">
          <Glyph m="breaking" />
          <span className="flex min-w-0 flex-col"><Inline text={lede(b.what) || b.what} />{b.callers.length > 0 && <span className="text-[13px] text-muted">Callers: {b.callers.join('; ')}</span>}</span>
        </li>
      ))}</ul>}
      {found && <Checker spec={spec} found={found} />}
    </section>
  );
}

const linesOf = (p: Pending) => (p.items.length
  ? p.items.map((i) => ({ key: i.ptr + i.line, breaking: i.breaking, text: i.line }))
  : (p.spec.changes ?? []).map((c) => ({ key: c.choice, breaking: false, text: lede(c.now) || c.choice })));

/** The confirmation before approving: one line per change, the breaks with their callers, and the judge's checks. */
export function ApproveSheet({ pending, busy, error, decide, onClose }: { pending: Pending } & Deciding) {
  const { spec, verdict, found, base, name } = pending;
  const lines = linesOf(pending);
  const actions = <><button type="button" onClick={onClose} className={sec}>Not yet</button><button type="button" disabled={!!busy} onClick={() => void decide('approve').then((ok) => ok && onClose())} className={prim}>{busy === 'approve' ? 'Approving…' : `Approve ${name}`}</button></>;
  return (
    <Sheet title={`Approve ${name}?`} onClose={onClose} actions={actions}>
      <p className="m-0 text-ink-2">{NOUN[spec.kind][2]} contract, measured against {base ?? 'no approved version'}.</p>
      {lines.length > 0 && <section className="flex flex-col gap-1">
        <h3 className={h3}>{lines.length} {lines.length === 1 ? 'change' : 'changes'}</h3>
        <ul className="m-0 flex list-none flex-col divide-y divide-line p-0">{lines.map((l) => <li key={l.key} className="flex items-baseline gap-2.5 py-2 [overflow-wrap:anywhere]"><Glyph m={l.breaking ? 'breaking' : 'changed'} /><Inline text={l.text} /></li>)}</ul>
      </section>}
      <BreakLines spec={spec} found={found} />
      <section className="flex flex-col">
        <h3 className={h3}>Automatic checks</h3>
        {verdict ? <CheckResult verdict={verdict} open={false} summary="Automatic checks passed" /> : <p className="m-0 flex min-h-11 items-center text-success">Automatic checks passed</p>}
      </section>
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
    </Sheet>
  );
}

/** Ask for changes: one reason the agent reads, sent as the rejection of this version. */
export function AskSheet({ name, busy, error, decide, onClose }: { name: string } & Deciding) {
  const [reason, setReason] = useState('');
  const form = useId();
  const actions = <><button type="button" onClick={onClose} className={sec}>Cancel</button><button type="submit" form={form} disabled={!!busy || !reason.trim()} className={prim}>{busy === 'reject' ? 'Sending…' : 'Send to the agent'}</button></>;
  return (
    <Sheet title={`Ask for changes to ${name}`} onClose={onClose} actions={actions}>
      <form id={form} className="flex flex-col gap-2" onSubmit={(e) => (e.preventDefault(), void decide('reject', reason.trim()).then((ok) => ok && onClose()))}>
        <label className="flex flex-col gap-1 text-[14px] text-ink-2">
          What should change?
          <textarea required data-autofocus value={reason} onChange={(e) => setReason(e.target.value)} rows={4} className="focus-ring rounded-lg border border-line bg-surface px-3 py-2 text-[15px] text-ink" />
        </label>
        <p className="m-0 text-[13px] text-muted">The agent reads this and drafts the next version. Nothing is approved.</p>
        {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
      </form>
    </Sheet>
  );
}
