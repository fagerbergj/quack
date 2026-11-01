import type { AssistantMessage, Message } from '@earendil-works/pi-ai';
import type { View } from './view.ts';

const text = (content: string | readonly { type: string; text?: string }[]) =>
  typeof content === 'string' ? content : content.map((c) => (c.type === 'text' ? c.text : `[${c.type}]`)).join('');

function Assistant({ m }: { m: AssistantMessage }) {
  return (
    <>
      {m.errorMessage && <p>{`Error: ${m.errorMessage}`}</p>}
      {m.content.map((b, i) => {
        if (b.type === 'text') return <p key={i}>{b.text}</p>;
        if (b.type === 'thinking') return <details key={i}><summary>Thinking</summary><p>{b.thinking}</p></details>;
        return <details key={i}><summary>{`Tool call: ${b.name}`}</summary><pre>{JSON.stringify(b.arguments, null, 2)}</pre></details>;
      })}
    </>
  );
}

function Line({ m }: { m: Message }) {
  if (m.role === 'user') return <article aria-label="You"><h3>You</h3><p>{text(m.content)}</p></article>;
  if (m.role === 'assistant') return <article aria-label="Assistant"><h3>Assistant</h3><Assistant m={m} /></article>;
  if (m.role === 'toolResult') {
    const label = `Tool result: ${m.toolName}${m.isError ? ' (error)' : ''}`;
    return <details><summary>{label}</summary><pre>{text(m.content)}</pre></details>;
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
