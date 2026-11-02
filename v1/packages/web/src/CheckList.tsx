import { useState } from 'react';
import { checkFailed, type Check } from './api.ts';
import { Markdown } from './Markdown.tsx';

// The judge prefixes a note with the unit it judged ("RB2: ..."); "card" and "facts" name the whole card, so they add nothing.
const WHOLE = new Set(['card', 'facts', 'review']);
const UNIT = /^([A-Za-z0-9_.-]{1,12}): ([\s\S]*)$/;

export function Note({ text }: { text: string }) {
  const [more, setMore] = useState(false);
  const m = UNIT.exec(text);
  const unit = m && !WHOLE.has(m[1]!) ? m[1]! : undefined;
  const body = m ? m[2]! : text;
  const first = /^[\s\S]+?[.!?](?=\s|$)/.exec(body)?.[0] ?? body;
  const long = first.length < body.length;
  return (
    <div className="flex flex-col items-start gap-1">
      {unit && <span className="rounded-md bg-soft px-1.5 py-0.5 text-[11px] font-medium text-ink-2">{unit}</span>}
      <div className="min-w-0 text-[12.5px] text-ink-2 [&_p]:m-0"><Markdown text={long && !more ? first : body} /></div>
      {long && <button type="button" onClick={() => setMore(!more)} className="min-h-11 cursor-pointer border-0 bg-transparent p-0 text-[12.5px] text-link hover:underline lg:min-h-0">{more ? 'Less' : 'More'}</button>}
    </div>
  );
}

/** The judge's notes for one check, behind a disclosure. */
export function Why({ check }: { check: Check }) {
  return (
    <details>
      <summary className="flex min-h-11 cursor-pointer items-center text-[12.5px] text-muted lg:min-h-6">Why</summary>
      <div className="flex flex-col gap-2 pb-1">{check.notes.length ? check.notes.map((n, i) => <Note key={i} text={n} />) : <p className="m-0 text-[12.5px] text-muted">No note from the judge.</p>}</div>
    </details>
  );
}

/** One failed check: its plain label, with the reason one tap away. */
export function FailedCheck({ check, tone }: { check: Check; tone: 'danger' | 'muted' }) {
  const box = tone === 'danger' ? 'border-danger-line bg-danger-bg/60 text-danger' : 'border-line bg-soft text-ink-2';
  return (
    <div className={`rounded-lg border px-3 py-1.5 ${box}`}>
      <p title={`${check.criterion}.${check.id}`} className="m-0 break-words font-medium">{check.label}</p>
      <Why check={check} />
    </div>
  );
}

export const failedChecks = (checks: Check[]) => checks.filter(checkFailed);
