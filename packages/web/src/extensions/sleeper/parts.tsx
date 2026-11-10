import { useState } from 'react';
import { StatusIcon } from '../../StatusIcon.tsx';
import { FailedCheck } from '../../CheckList.tsx';
import { checkFailed, type SealedArtifact } from '../../api.ts';
import { Markdown } from '../../Markdown.tsx';

export type News = Array<{ title: string; url: string; date: string; player: string }>;

const ET = 'America/New_York';
export const when = (iso: string | number) => new Intl.DateTimeFormat('en-US', { timeZone: ET, weekday: 'short', hour: 'numeric', minute: '2-digit', timeZoneName: 'short' }).format(new Date(iso));
export const weekday = (ms: number) => new Intl.DateTimeFormat('en-US', { timeZone: ET, weekday: 'short' }).format(ms);

export const slot = (ms: number) => `${weekday(ms)} ${new Intl.DateTimeFormat('en-US', { timeZone: ET, hour: 'numeric', minute: '2-digit' }).format(ms)}`;

// A failed check is a status line naming the checks that missed; a pass draws nothing here, its tick sits on the title (TitleTick).
export function Verdict({ artifact }: { artifact: SealedArtifact }) {
  const v = artifact.verdict;
  if (!v || v.passed) return null;
  return (
    <div className="flex min-w-0 flex-col gap-1.5 text-[12px]">
      <div className="flex items-center gap-2"><StatusIcon verdict={v} /><span className="font-medium text-ink-2">Didn't pass its checks</span></div>
      {v.checks.filter(checkFailed).map((c) => <FailedCheck key={`${c.criterion}.${c.id}`} check={c} />)}
    </div>
  );
}

/** A passed double-check as a green tick beside a card's title; checks it partly missed are named in its alt text. */
export function TitleTick({ artifact }: { artifact: SealedArtifact }) {
  const v = artifact.verdict;
  if (!v?.passed) return null;
  const missed = v.checks.filter(checkFailed).map((c) => c.label);
  return <StatusIcon verdict={v} label={missed.length ? `Double-checked; partly missed: ${missed.join('; ')}` : undefined} />;
}

const host = (url: string) => new URL(url, 'http://x').hostname.replace(/^www\./, '');

// Grouped by player only when more than one is named.
export function NewsList({ items }: { items: News }) {
  if (!items.length) return null;
  const names = [...new Set(items.map((n) => n.player))];
  return (
    <details className="rounded-lg border border-line bg-surface px-3 py-1.5 text-[12px]">
      <summary className="flex min-h-11 cursor-pointer list-none items-center font-medium text-ink-2 lg:min-h-6 [&::-webkit-details-marker]:hidden">{`News (${items.length})`}</summary>
      {names.map((name) => (
        <div key={name} className="mt-1 flex flex-col">
          {names.length > 1 && <p className="m-0 font-semibold text-ink-2">{name}</p>}
          {items.filter((n) => n.player === name).map((n) => (
            <a key={n.url} href={n.url} target="_blank" rel="noreferrer" className="flex min-h-11 min-w-0 flex-col justify-center break-words lg:min-h-0 lg:py-1">
              {n.title}
              <span className="text-muted">{`${host(n.url)} · ${n.date.slice(0, 10)}`}</span>
            </a>
          ))}
        </div>
      ))}
    </details>
  );
}

/** Model prose as markdown, cut to its first sentence behind More; `whole` shows it all. */
export function Blurb({ text, whole }: { text: string; whole?: boolean }) {
  const [more, setMore] = useState(false);
  const first = /^[\s\S]+?(?<!\b(?:e\.g|i\.e))[.!?](?=\s|$)/.exec(text)?.[0] ?? text;
  const long = !whole && first.length < text.length;
  return (
    <div className="flex min-w-0 flex-col items-start gap-1 text-[13px] text-ink-2 [&_p]:m-0">
      <Markdown text={long && !more ? first : text} />
      {long && <button type="button" onClick={() => setMore(!more)} className="min-h-11 min-w-11 cursor-pointer border-0 bg-transparent p-0 text-left text-[12.5px] text-link hover:underline lg:min-h-0 lg:min-w-0">{more ? 'Less' : 'More'}</button>}
    </div>
  );
}
