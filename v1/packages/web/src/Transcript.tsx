import { useState } from 'react';
import type { AssistantMessage, Message } from '@earendil-works/pi-ai';
import type { View } from './view.ts';

const text = (content: string | readonly { type: string; text?: string }[]) =>
  typeof content === 'string' ? content : content.map((c) => (c.type === 'text' ? c.text : `[${c.type}]`)).join('');

const LIMIT = 2000;
const clip = (s: string, n: number) => (s.length > n ? `${s.slice(0, n)}...` : s);

// The first string or string-list argument is usually the query, URL or command.
function hint(args: Record<string, unknown>) {
  const v = Object.values(args).find((a) => typeof a === 'string' || (Array.isArray(a) && a.every((x) => typeof x === 'string')));
  return v === undefined ? '' : clip([v].flat().join(', '), 80);
}

function Tool({ label, note, body }: { label: string; note: string; body: string }) {
  const [all, setAll] = useState(false);
  const long = body.length > LIMIT;
  return (
    <details>
      <summary><span>{label}</span> <span>{note}</span></summary>
      <pre>{all || !long ? body : clip(body, LIMIT)}</pre>
      {long && !all && <button type="button" onClick={() => setAll(true)}>{`Show all (${body.length} characters)`}</button>}
    </details>
  );
}

function Assistant({ m }: { m: AssistantMessage }) {
  return (
    <>
      {m.errorMessage && <p>{`Error: ${m.errorMessage}`}</p>}
      {m.content.map((b, i) => {
        if (b.type === 'text') return <p key={i}>{b.text}</p>;
        if (b.type === 'thinking') return b.thinking.trim() ? <details key={i}><summary>Thinking</summary><p>{b.thinking}</p></details> : null;
        return <Tool key={i} label={`Tool call: ${b.name}`} note={hint(b.arguments)} body={JSON.stringify(b.arguments, null, 2)} />;
      })}
    </>
  );
}

function Line({ m }: { m: Message }) {
  if (m.role === 'user') return <article aria-label="You"><h3>You</h3><p>{text(m.content)}</p></article>;
  if (m.role === 'assistant') return <article aria-label="Assistant"><h3>Assistant</h3><Assistant m={m} /></article>;
  if (m.role === 'toolResult') {
    const label = `Tool result: ${m.toolName}${m.isError ? ' (error)' : ''}`;
    const body = text(m.content);
    return <Tool label={label} note={m.isError ? clip(body.split('\n')[0] ?? '', 80) : `${body.length} characters`} body={body} />;
  }
  return null;
}

export function Transcript({ view }: { view: View }) {
  return (
    <div role="log" aria-label="Transcript">
      {view.entries.flatMap((e) => (e.model ?? []).map((m, i) => <Line key={`${e.id}:${i}`} m={m} />))}
      {view.partial && <Line m={view.partial} />}
    </div>
  );
}
