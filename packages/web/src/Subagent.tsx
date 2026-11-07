import { useContext, useEffect, useState } from 'react';
import { json } from './api.ts';
import { SessionKeyContext } from './extension.ts';
import { Markdown } from './Markdown.tsx';

type Message = { role: 'user' | 'assistant'; text: string };

const idOf = (details: unknown) => (details as { conversationId?: unknown } | null | undefined)?.conversationId;

function Child({ sessionKey, id }: { sessionKey: string; id: string }) {
  const [messages, setMessages] = useState<Message[]>();
  const [error, setError] = useState<string>();
  useEffect(() => {
    fetch(`sessions/${encodeURIComponent(sessionKey)}/messages?conversation=${encodeURIComponent(id)}&limit=50`)
      .then((r) => json<{ messages: Message[] }>(r))
      .then((body) => setMessages(body.messages))
      .catch((err: Error) => setError(err.message));
  }, [sessionKey, id]);
  if (error) return <p className="m-0 text-[13px] text-danger">{`Could not load the subagent: ${error}`}</p>;
  if (!messages) return <p className="m-0 text-[13px] text-muted">Loading…</p>;
  return (
    <ol className="m-0 flex list-none flex-col gap-3 p-0">
      {messages.map((m, i) => (
        <li key={i}>
          <p className="m-0 text-[12px] text-muted">{m.role === 'user' ? 'Task' : 'Subagent'}</p>
          <Markdown text={m.text} />
        </li>
      ))}
    </ol>
  );
}

/** A subagent call's child messages, nested under the call; fetched when opened. Nothing when the call recorded no child. */
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
