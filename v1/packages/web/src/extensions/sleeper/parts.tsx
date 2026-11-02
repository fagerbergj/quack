import { useState } from 'react';
import { StatusIcon } from '../../Artifacts.tsx';
import { FailedCheck, failedChecks } from '../../CheckList.tsx';
import type { SealedArtifact } from '../../api.ts';
import { Markdown } from '../../Markdown.tsx';

export type News = Array<{ title: string; url: string; date: string; player: string }>;

const ET = 'America/New_York';
export const when = (iso: string | number) => new Intl.DateTimeFormat('en-US', { timeZone: ET, weekday: 'short', hour: 'numeric', minute: '2-digit', timeZoneName: 'short' }).format(new Date(iso));
export const weekday = (ms: number) => new Intl.DateTimeFormat('en-US', { timeZone: ET, weekday: 'short' }).format(ms);

// `brief` is the strip's status; the card also lists what missed. A pass is only a tick, and a partial miss on a pass stays collapsed.
export function Verdict({ artifact, brief }: { artifact: SealedArtifact; brief?: boolean }) {
  const v = artifact.verdict;
  if (!v) return null;
  const missed = brief ? [] : failedChecks(v.checks);
  return (
    <div className="flex min-w-0 flex-col gap-1.5 text-[12px]">
      <div className="flex items-center gap-2">
        <StatusIcon verdict={v} />
        {!v.passed && <span className="font-medium text-ink-2">Didn't pass its checks</span>}
      </div>
      {v.passed && missed.length > 0 && (
        <details>
          <summary className="flex min-h-11 cursor-pointer items-center text-muted lg:min-h-6">{`${missed.length} minor ${missed.length === 1 ? 'note' : 'notes'}`}</summary>
          <div className="flex flex-col gap-1.5">{missed.map((c) => <FailedCheck key={`${c.criterion}.${c.id}`} check={c} tone="muted" />)}</div>
        </details>
      )}
      {!v.passed && missed.map((c) => <FailedCheck key={`${c.criterion}.${c.id}`} check={c} tone="danger" />)}
    </div>
  );
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
