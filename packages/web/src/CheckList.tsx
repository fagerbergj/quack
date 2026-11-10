import { checkFailed, type Check } from './api.ts';
import { EXTENSIONS } from './extensions/index.ts';
import { Blurb } from './Markdown.tsx';

// The judge prefixes a note with the unit it judged ("RB2: ..."); a prefix equal to the check's scope names the whole artifact, so it adds nothing.
const wholeUnits = () => EXTENSIONS.flatMap((e) => e.wholeUnits ?? []);
const UNIT = /^([A-Za-z0-9_.-]{1,12}): ([\s\S]*)$/;

export function Note({ text, scope }: { text: string; scope?: string }) {
  const m = UNIT.exec(text);
  const unit = m && m[1] !== scope && !wholeUnits().includes(m[1]!) ? m[1] : undefined;
  const body = m ? m[2]! : text;
  return (
    <div className="flex flex-col items-start gap-1">
      {unit && <span className="rounded-md bg-soft px-1.5 py-0.5 text-[11px] font-medium text-ink-2">{unit}</span>}
      <Blurb text={body} className="text-[12.5px]" />
    </div>
  );
}

export function Pill({ check }: { check: Check }) {
  const [text, tone] = check.met === null ? ["Didn't apply", 'bg-soft text-muted'] : !checkFailed(check) ? ['Pass', 'bg-success-bg text-success'] : ['Fail', 'bg-danger-bg text-danger'];
  // Only a partial result says how much: 0% on a pass (a pitfall that didn't show up) or 100% on a pass reads as noise.
  const score = check.met !== null && check.met > 0 && check.met < 1 ? ` · ${Math.round(check.met * 100)}%` : '';
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
export function FailedCheck({ check }: { check: Check }) {
  return (
    <div className="rounded-lg border border-danger-line bg-danger-bg/60 px-3 text-danger">
      <CheckRow check={check} />
    </div>
  );
}
