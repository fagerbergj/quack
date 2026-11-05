import { useContext, useEffect, useState } from 'react';
import type { TranscriptItem } from '@earendil-works/pi-durable';
import { json } from './api.ts';
import { SessionKeyContext } from './extension.ts';
import { Transcript } from './Transcript.tsx';
import { emptyView } from './view.ts';

const idOf = (details: unknown) => (details as { conversationId?: unknown } | null | undefined)?.conversationId;

function Child({ sessionKey, id }: { sessionKey: string; id: string }) {
  const [items, setItems] = useState<readonly TranscriptItem[]>();
  const [error, setError] = useState<string>();
  useEffect(() => {
    fetch(`sessions/${encodeURIComponent(sessionKey)}/transcript?conversation=${encodeURIComponent(id)}`)
      .then((r) => json<{ transcript: Array<{ id: number; kind: string; model: unknown }> }>(r))
      .then(({ transcript }) => setItems(transcript.map((entry) => ({ kind: 'entry', entry }) as unknown as TranscriptItem)))
      .catch((err: Error) => setError(err.message));
  }, [sessionKey, id]);
  if (error) return <p className="m-0 text-[13px] text-danger">{`Could not load the subagent: ${error}`}</p>;
  return items ? <Transcript view={{ ...emptyView, items }} load={() => undefined} /> : <p className="m-0 text-[13px] text-muted">Loading…</p>;
}

/** A subagent call's child transcript, nested under the call; fetched when opened. Nothing when the call recorded no child. */
export function SubagentTranscript({ details }: { details: unknown }) {
  const sessionKey = useContext(SessionKeyContext);
  const [opened, setOpened] = useState(false);
  const id = idOf(details);
  if (!sessionKey || typeof id !== 'string') return null;
  return (
    <details onToggle={(e) => e.currentTarget.open && setOpened(true)} className="text-[13px]">
      <summary className="min-h-11 cursor-pointer text-muted hover:text-ink lg:min-h-0">Subagent transcript</summary>
      <div className="ml-1.5 border-l-2 border-line pl-4">{opened && <Child sessionKey={sessionKey} id={id} />}</div>
    </details>
  );
}
