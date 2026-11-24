import type { ReactNode } from 'react';
import { checkFailed, shortOf, type Check } from './api.ts';
import { EXTENSIONS } from './extensions/index.ts';
import { Blurb } from './Markdown.tsx';

// The judge prefixes a note with the unit it judged ("RB2: ..."); a prefix equal to the check's scope names the whole artifact, so it adds nothing.
const wholeUnits = () => EXTENSIONS.flatMap((e) => e.wholeUnits ?? []);
const UNIT = /^([A-Za-z0-9_.-]{1,12}): ([\s\S]*)$/;

/** The version a verdict judged, so an extension can read a note's item against it. */
export type About = { kind: string; content?: string };

export function Note({ text, scope, about }: { text: string; scope?: string; about?: About }) {
  const drawn = about?.content === undefined ? undefined : EXTENSIONS.map((e) => e.note?.(about.kind, about.content!, text)).find(Boolean);
  if (drawn) return drawn;
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

/** A check's result as one mark: passed, failed, or didn't apply. */
export function Mark({ check }: { check: Check }) {
  const [glyph, tone, word] = check.met === null ? ['–', 'font-normal text-muted', "Didn't apply"] : checkFailed(check) ? ['✗', 'text-danger', 'Failed'] : ['✓', 'text-success', 'Passed'];
  return <span role="img" aria-label={word} title={word} className={`w-4 flex-none text-center font-bold ${tone}`}>{glyph}</span>;
}

// How many items fell short on screen ("3 of 60"); the words go to assistive tech and the tooltip.
function Short({ check }: { check: Check }) {
  if (check.met === null) return <span aria-hidden="true" className="flex-none text-[11px] text-muted">Didn't apply</span>;
  const s = checkFailed(check) ? shortOf(check) : undefined;
  if (!s) return null;
  const how = `${s.n} of ${s.of} ${check.groups ? 'groups' : 'items'} fell short`;
  return <span title={how} className="flex-none text-[12px] text-danger tabular-nums"><span aria-hidden="true">{`${s.n} of ${s.of}`}</span><span className="sr-only">{how}</span></span>;
}

/** Settled items by who answered them: `code` is the judge's own, and any model but the decision model is the judge model an item escalated to. */
export function byWhom(resolvers: Record<string, number> = {}) {
  const out = { clef: 0, judge: 0, code: 0 };
  // shortcut: the decision model is told apart by "clef" in its id, as every deployment names it; upgrade if a verdict names its decision model.
  for (const [k, n] of Object.entries(resolvers)) out[k === 'code' ? 'code' : /clef/i.test(k) ? 'clef' : 'judge'] += n;
  return out;
}

/** "60 items · 52 by Clef, 8 by the judge model", with each resolver's own id in the tooltip; nothing for a check of one item. */
export function resolverLine(c: Pick<Check, 'units' | 'resolvers' | 'reused'>): { text: string; tip: string } | undefined {
  if (!c.units || c.units < 2 || !c.resolvers) return undefined;
  const w = byWhom(c.resolvers);
  const reused = Object.keys(c.reused ?? {}).length;
  const parts = [w.clef && `${w.clef} by Clef`, w.judge && `${w.judge} by the judge model`, w.code && `${w.code} by code`, reused && `${reused} from earlier versions`].filter(Boolean);
  const tip = Object.entries(c.resolvers).map(([k, n]) => `${n} by ${k}`).join(', ');
  return { text: [`${c.units} items`, parts.join(', ')].filter(Boolean).join(' · '), tip: tip || 'No item was asked' };
}

const FIRST = 5;
const chevron = <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" className="chev flex-none text-muted transition-transform"><path d="M9 6l6 6-6 6" /></svg>;
const summary = 'flex min-h-11 cursor-pointer list-none items-center gap-2 lg:min-h-8 [&::-webkit-details-marker]:hidden';

// The first failing items open with the check; the rest wait behind one more fold.
function Notes({ notes }: { notes: ReactNode[] }) {
  if (notes.length <= FIRST) return notes;
  return (
    <>
      {notes.slice(0, FIRST)}
      <details>
        <summary className={`${summary} text-muted hover:text-ink`}>{chevron}{`${notes.length - FIRST} more failing`}</summary>
        <div className="flex flex-col gap-2 pt-1">{notes.slice(FIRST)}</div>
      </details>
    </>
  );
}

/** One check: its mark, label and fail count, expanding to its failing items and who settled its items. A failed check starts open. */
export function CheckRow({ check, mark, about }: { check: Check; mark?: boolean; about?: About }) {
  const line = resolverLine(check);
  const head = (
    <>
      {mark && <Mark check={check} />}
      <span title={`${check.criterion}.${check.id}`} className="min-w-0 flex-1 break-words">{check.label}</span>
      {mark && <Short check={check} />}
    </>
  );
  if (!check.notes.length && !line) return <div className="flex min-h-11 items-center gap-2 lg:min-h-8">{head}{mark && <span aria-hidden="true" className="w-3 flex-none" />}</div>;
  return (
    <details open={checkFailed(check)}>
      <summary className={summary}>{head}{chevron}</summary>
      <div className={`flex flex-col gap-2 pb-2 ${mark ? 'pl-6' : ''}`}>
        <Notes notes={check.notes.map((n, i) => <Note key={i} text={n} scope={check.scope} about={about} />)} />
        {line && <p title={line.tip} className="m-0 text-[12px] text-muted">{line.text}</p>}
      </div>
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
