import { PrChip } from './prState.tsx';
import { headMeta, parseOpening, type PrMeta } from './PrCard.tsx';
import { prOf } from './pulls.ts';

function Changes({ files, url }: { files: NonNullable<PrMeta['files']>; url: string }) {
  const add = files.reduce((n, f) => n + f.additions, 0);
  const del = files.reduce((n, f) => n + f.deletions, 0);
  return (
    <>
      <dt className="text-muted">Changes</dt>
      <dd className="m-0 flex flex-col gap-1">
        <span className="flex flex-col tabular-nums">
          <span className="whitespace-nowrap">{`${files.length} ${files.length === 1 ? 'file' : 'files'} changed`}</span>
          <span className="whitespace-nowrap" aria-label={`${add} added, ${del} removed`}><span className="text-success">{`+${add}`}</span>{' '}<span className="text-danger">{`−${del}`}</span></span>
        </span>
        <a href={url} target="_blank" rel="noopener noreferrer" className="flex min-h-11 items-center text-link no-underline hover:underline lg:min-h-0">Open on GitHub ↗</a>
      </dd>
    </>
  );
}

/** The pull request a review session is about: state, title, branches and size, from the opening message and the service's meta; a row with no data is left out. */
export function PrSection({ sessionKey, opening, meta, title }: { sessionKey: string; title: string; opening?: string; meta: readonly Record<string, unknown>[] }) {
  const key = prOf(sessionKey);
  if (!key) return null;
  const { repo, number } = key;
  const o = opening ? parseOpening(opening) : undefined;
  const pr = headMeta(meta, o);
  return (
    <section aria-labelledby="rail-pr">
      <h2 id="rail-pr" className="m-0 mb-3 text-xs font-semibold tracking-[.06em] text-muted uppercase">Pull request</h2>
      <div className="-mx-3 flex flex-col items-start gap-3 rounded-2xl bg-soft px-3 py-3.5">
        <PrChip sessionKey={sessionKey} />
        <p className="m-0 text-[14.5px] leading-snug font-semibold [overflow-wrap:anywhere]">{`#${number} ${pr?.title ?? o?.title ?? title}`}</p>
        {(o?.headRef || o?.baseRef) && <code className="font-mono text-[12px] text-ink-2 [overflow-wrap:anywhere]">{[o.headRef, o.baseRef].filter(Boolean).join(' → ')}</code>}
        {pr?.files?.length ? (
          <dl className="m-0 grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-[13px]">
            <Changes files={pr.files} url={`https://github.com/${repo}/pull/${number}`} />
          </dl>
        ) : null}
      </div>
    </section>
  );
}
