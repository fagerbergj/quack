import { useEffect, useState } from 'react';
import type { SealedArtifact } from './api.ts';
import { artifactCard, artifactName } from './Artifacts.tsx';
import { CheckResult } from './CheckResult.tsx';
import { delivered, versionOf } from './delivered.ts';
import { SessionKeyContext } from './extension.ts';
import { Content } from './JsonView.tsx';
import { Loaded, useContent } from './useContent.tsx';
import { FollowNewest, iconButton, verdictMark, VersionMenu, type VersionItem } from './VersionMenu.tsx';

/** The versions the pane offers and the one it shows: the focused artifact's kind, else the kind sealed last; the focused version, else the newest. */
export function paneVersions(items: SealedArtifact[], focus?: SealedArtifact): { versions: SealedArtifact[]; a: SealedArtifact } | undefined {
  if (!items.length) return undefined;
  const kind = focus?.kind ?? items.reduce((n, a) => (a.sealedAt >= n.sealedAt ? a : n)).kind;
  const versions = delivered(items.filter((a) => a.kind === kind));
  return { versions, a: (focus && versions.find((v) => versionOf(v) === versionOf(focus))) ?? versions[0]! };
}

const itemsOf = (versions: SealedArtifact[]): VersionItem[] => versions.map((v) => ({ id: versionOf(v), short: `v${versionOf(v)}`, ...verdictMark(v.verdict) })).reverse();

/** Copies a version's raw content (Markdown for a report); the icon turns to a tick and a status says "Copied" for two seconds. */
function CopyButton({ text }: { text?: string }) {
  const [copied, setCopied] = useState<'ok' | 'failed'>();
  useEffect(() => {
    if (!copied) return;
    const t = setTimeout(() => setCopied(undefined), 2000);
    return () => clearTimeout(t);
  }, [copied]);
  const say = copied === 'ok' ? 'Copied' : copied === 'failed' ? 'Could not copy' : 'Copy';
  return (
    <>
      <button type="button" aria-label="Copy" title={say} disabled={text === undefined} onClick={() => navigator.clipboard.writeText(text ?? '').then(() => setCopied('ok'), () => setCopied('failed'))} className={iconButton}>
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{copied === 'ok' ? <path d="M5 12.5l4.5 4.5L19 7.5" /> : <><rect x="9" y="9" width="11" height="11" rx="2" /><path d="M5 15V6a2 2 0 0 1 2-2h9" /></>}</svg>
      </button>
      <span role="status" className="sr-only">{copied ? say : ''}</span>
    </>
  );
}

function Body({ a, now }: { a: SealedArtifact; now?: { running: boolean; meta: readonly Record<string, unknown>[] } }) {
  return artifactCard(a, now) ?? (
    <div className="rounded-[22px] border border-line bg-surface p-5 text-sm leading-relaxed text-ink-2 shadow-card [overflow-wrap:anywhere]">
      <p className="m-0 mb-3"><span className="rounded-md bg-soft px-2 py-0.5 text-xs text-ink-2">{artifactName(a)}</span></p>
      <Content text={a.content ?? ''} />
    </div>
  );
}

export const paneLayout = 'mx-auto flex w-full max-w-[1100px] min-w-0 flex-col gap-4 px-4 pt-4 pb-10 sm:px-5';

/** One artifact with its version row, as the session pane and the artifact's own page both draw it; `now` is the live run, for the newest version. */
export function ArtifactView({ sessionKey, versions, a, onPick, onFollow, onClose, now }: { sessionKey: string; versions: SealedArtifact[]; a: SealedArtifact; onPick: (v: SealedArtifact) => void; onFollow?: () => void; onClose?: () => void; now?: { running: boolean; meta: readonly Record<string, unknown>[] } }) {
  const pick = (n?: number) => { const v = versions.find((x) => versionOf(x) === n); if (v) onPick(v); };
  const closeLabel = `Close ${artifactName(a)}`;
  const { content } = useContent(sessionKey, a);
  return (
    <div className={paneLayout}>
      <div className="flex items-center gap-1.5">
        <VersionMenu name="Version" items={itemsOf(versions)} value={versionOf(a)} onPick={pick} />
        <span className="flex-1" />
        <CopyButton text={content} />
        {onFollow && <FollowNewest onClick={onFollow} />}
        {onClose && (
          <button type="button" aria-label={closeLabel} title={closeLabel} onClick={onClose} className={iconButton}>
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18" /></svg>
          </button>
        )}
      </div>
      <SessionKeyContext value={sessionKey}><Loaded sessionKey={sessionKey} a={a} what={`version ${versionOf(a)}`}>{(full) => <Body a={full} now={a === versions[0] ? now : undefined} />}</Loaded></SessionKeyContext>
      {a.verdict && <CheckResult verdict={a.verdict} open={!a.verdict.passed} about={{ kind: a.kind, content }} />}
    </div>
  );
}
