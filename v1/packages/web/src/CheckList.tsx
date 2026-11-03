import { useState } from 'react';
import { checkFailed, type Check } from './api.ts';
import { EXTENSIONS } from './extensions/index.ts';
import { Markdown } from './Markdown.tsx';

// The judge prefixes a note with the unit it judged ("RB2: ..."); a prefix equal to the check's scope names the whole artifact, so it adds nothing.
const wholeUnits = () => EXTENSIONS.flatMap((e) => e.wholeUnits ?? []);
const UNIT = /^([A-Za-z0-9_.-]{1,12}): ([\s\S]*)$/;

export function Note({ text, scope }: { text: string; scope?: string }) {
  const [more, setMore] = useState(false);
  const m = UNIT.exec(text);
  const unit = m && m[1] !== scope && !wholeUnits().includes(m[1]!) ? m[1] : undefined;
  const body = m ? m[2]! : text;
  const first = /^[\s\S]+?(?<!\b(?:e\.g|i\.e))[.!?](?=\s|$)/.exec(body)?.[0] ?? body;
  const long = first.length < body.length;
  return (
    <div className="flex flex-col items-start gap-1">
      {unit && <span className="rounded-md bg-soft px-1.5 py-0.5 text-[11px] font-medium text-ink-2">{unit}</span>}
      <div className="min-w-0 text-[12.5px] text-ink-2 [&_p]:m-0"><Markdown text={long && !more ? first : body} /></div>
      {long && <button type="button" onClick={() => setMore(!more)} className="min-h-11 min-w-11 cursor-pointer border-0 bg-transparent p-0 text-left text-[12.5px] text-link hover:underline lg:min-h-0 lg:min-w-0">{more ? 'Less' : 'More'}</button>}
    </div>
  );
}

export function Pill({ check }: { check: Check }) {
  const [text, tone] = check.met === null ? ["Didn't apply", 'bg-soft text-muted'] : !checkFailed(check) ? ['Pass', 'bg-success-bg text-success'] : ['Fail', 'bg-danger-bg text-danger'];
  const score = check.met === null ? '' : ` · ${Math.round(check.met * 100)}%`;
  return <span className={`flex-none rounded-full px-2 py-0.5 text-[11px] font-semibold ${tone}`}>{text + score}</span>;
}

/** One check: its label (and result), expanding to the judge's notes. A failed check starts open; one with no note has nothing to expand. */
export function CheckRow({ check, pill }: { check: Check; pill?: boolean }) {
  const head = (
    <>
      <span title={`${check.criterion}.${check.id}`} className="min-w-0 flex-1 break-words">{check.label}</span>
      {pill && <Pill check={check} />}
    </>
  );
  if (!check.notes.length) return <div className="flex min-h-11 items-center gap-2 lg:min-h-8">{head}{pill && <span aria-hidden="true" className="w-3 flex-none" />}</div>;
  return (
    <details open={checkFailed(check)}>
      <summary className="flex min-h-11 cursor-pointer list-none items-center gap-2 lg:min-h-8 [&::-webkit-details-marker]:hidden">
        {head}
        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" className="chev flex-none text-muted transition-transform"><path d="M9 6l6 6-6 6" /></svg>
      </summary>
      <div className="flex flex-col gap-2 pb-2">{check.notes.map((n, i) => <Note key={i} text={n} scope={check.scope} />)}</div>
    </details>
  );
}

/** One failed check on a card, open on its reason. */
export function FailedCheck({ check, tone }: { check: Check; tone: 'danger' | 'muted' }) {
  const box = tone === 'danger' ? 'border-danger-line bg-danger-bg/60 text-danger' : 'border-line bg-soft text-ink-2';
  return (
    <div className={`rounded-lg border px-3 ${box}`}>
      <CheckRow check={check} />
    </div>
  );
}

export const failedChecks = (checks: Check[]) => checks.filter(checkFailed);
