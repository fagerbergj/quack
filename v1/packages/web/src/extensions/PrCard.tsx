import { useState, type ReactNode } from 'react';
import { Markdown } from '../Markdown.tsx';

export type PrMeta = {
  id: string; headSha?: string;
  files?: Array<{ path: string; additions: number; deletions: number }>;
  checks?: Array<{ name: string; status: string; conclusion: string | null }>;
  linked?: Array<{ number: number; title: string }>;
};
export type Opening = { repo: string; number: string; why: string; title: string; head?: string; headRef?: string; baseRef?: string; body: string };

// The trigger text the reviewer is given (github webhook.ts); the head line is absent when a comment started the review.
const OPENING = /^Review pull request ([^#\s]+)#(\d+) \((.*?)\)\.\nTitle: (.*)\n(?:Head: (\S+) on (.*?); base: (.*)\n)?\n([\s\S]*)$/;

export function parseOpening(text: string): Opening | undefined {
  const m = OPENING.exec(text);
  return m ? { repo: m[1]!, number: m[2]!, why: m[3]!, title: m[4]!, head: m[5], headRef: m[6], baseRef: m[7], body: m[8]!.trim() } : undefined;
}

const FAILING = new Set(['failure', 'timed_out', 'cancelled', 'action_required', 'startup_failure']);
const SKIPPED = new Set(['skipped', 'neutral']);
export const checkState = (c: NonNullable<PrMeta['checks']>[number]) => (c.status !== 'completed' ? 'running' : FAILING.has(c.conclusion ?? '') ? 'failing' : SKIPPED.has(c.conclusion ?? '') ? 'skipped' : 'passing');

const row = 'flex min-h-11 cursor-pointer list-none items-center gap-2 border-t border-line px-3.5 text-[13.5px] text-ink-2 lg:min-h-9 [&::-webkit-details-marker]:hidden';
const chip = 'rounded-lg bg-soft px-2 py-0.5 font-mono text-[12px] [overflow-wrap:anywhere]';

function Section({ name, hint, open, children }: { name: string; hint?: ReactNode; open?: boolean; children: ReactNode }) {
  return (
    <details open={open}>
      <summary className={row}>
        <b className="font-semibold text-ink">{name}</b>
        {hint && <span className="min-w-0 flex-1 truncate">{hint}</span>}
        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" className="chev ml-auto flex-none text-muted transition-transform"><path d="M9 6l6 6-6 6" /></svg>
      </summary>
      {children}
    </details>
  );
}

const CLAMP = 400;

function Description({ body }: { body: string }) {
  const [all, setAll] = useState(false);
  const long = body.length > CLAMP;
  return (
    <div className="px-3.5 pb-3 text-[13.5px] leading-normal text-ink">
      <div className={long && !all ? 'max-h-44 overflow-hidden' : undefined}><Markdown text={body} /></div>
      {long && <button type="button" onClick={() => setAll(!all)} className="min-h-11 cursor-pointer border-0 bg-transparent p-0 text-[13px] text-link lg:min-h-0">{all ? 'Show less' : 'Show all'}</button>}
    </div>
  );
}

function Changes({ files }: { files: NonNullable<PrMeta['files']> }) {
  const add = files.reduce((n, f) => n + f.additions, 0);
  const del = files.reduce((n, f) => n + f.deletions, 0);
  return (
    <Section name="Changes" hint={<>{`${files.length} ${files.length === 1 ? 'file' : 'files'} `}<span className="text-success">{`+${add}`}</span>{' '}<span className="text-danger">{`−${del}`}</span></>} open>
      <ul className="m-0 list-none p-0 pb-2">
        {files.map((f) => (
          <li key={f.path} className="flex gap-2 px-3.5 py-1.5 text-[12.5px]">
            <span className="min-w-0 flex-1 truncate font-mono text-ink" title={f.path}>{f.path}</span>
            <span className="text-success">{`+${f.additions}`}</span><span className="text-danger">{`−${f.deletions}`}</span>
          </li>
        ))}
      </ul>
    </Section>
  );
}

function Checks({ checks }: { checks: NonNullable<PrMeta['checks']> }) {
  const by = Map.groupBy(checks, checkState);
  const hint = (['passing', 'failing', 'running', 'skipped'] as const).filter((s) => by.has(s)).map((s) => `${by.get(s)!.length} ${s}`).join(' · ');
  return (
    <Section name="Checks" hint={hint}>
      <ul className="m-0 flex list-none flex-col gap-1 p-0 px-3.5 pb-3 text-[12.5px]">
        {checks.map((c, i) => <li key={i} className="flex gap-2"><span className="min-w-0 flex-1 break-words">{c.name}</span><span className={checkState(c) === 'failing' ? 'text-danger' : 'text-muted'}>{checkState(c)}</span></li>)}
      </ul>
    </Section>
  );
}

function Linked({ repo, issues }: { repo: string; issues: NonNullable<PrMeta['linked']> }) {
  return (
    <Section name={issues.length > 1 ? 'Linked issues' : 'Linked issue'} hint={`#${issues[0]!.number} ${issues[0]!.title}`}>
      <ul className="m-0 flex list-none flex-col p-0 px-3.5 pb-2 text-[13px]">
        {issues.map((i) => <li key={i.number}><a href={`https://github.com/${repo}/issues/${i.number}`} target="_blank" rel="noopener noreferrer" className="flex min-h-11 items-center lg:min-h-8">{`#${i.number} ${i.title}`}</a></li>)}
      </ul>
    </Section>
  );
}

/** The first message of a review session as the pull request it is about; `meta` adds the files, checks and linked issues. */
export function PrCard({ o, meta }: { o: Opening; meta?: PrMeta }) {
  const url = `https://github.com/${o.repo}/pull/${o.number}`;
  return (
    <section aria-label="Pull request" className="overflow-hidden rounded-[20px] border border-line bg-surface shadow-card">
      <div className="flex flex-col gap-1.5 px-3.5 pt-3.5 pb-2.5">
        <span className="flex items-center gap-2 text-[12px] text-ink-2">
          <span className="min-w-0 flex-1 truncate">{`${o.repo} · ${o.why}`}</span>
          <a href={url} target="_blank" rel="noopener noreferrer" className="flex min-h-11 flex-none items-center px-1 lg:min-h-0">GitHub ↗</a>
        </span>
        <h2 className="m-0 text-[17px] leading-snug">{o.title}</h2>
        {(o.headRef || o.baseRef) && (
          <span className="flex flex-wrap items-center gap-1.5 text-[12px] text-ink-2">
            {o.headRef && <span className={chip}>{o.headRef}</span>}
            {o.headRef && o.baseRef && <span aria-hidden="true">→</span>}
            {o.baseRef && <span className={chip}>{o.baseRef}</span>}
            {o.head && <span className="font-mono text-muted">{o.head.slice(0, 7)}</span>}
          </span>
        )}
      </div>
      {o.body && <Section name="Description" open><Description body={o.body} /></Section>}
      {meta?.files?.length ? <Changes files={meta.files} /> : null}
      {meta?.checks?.length ? <Checks checks={meta.checks} /> : null}
      {meta?.linked?.length ? <Linked repo={o.repo} issues={meta.linked} /> : null}
    </section>
  );
}
