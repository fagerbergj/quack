import { useEffect, useState } from 'react';
import { answerQuestion, type Answer, type Question } from './api.ts';
import { Markdown } from './Markdown.tsx';
import { age, needId } from './sessions.ts';

const YOU_DECIDE = 'You decide';

// Titles allow inline `code` only; anything richer is the detail's job.
function Headline({ text }: { text: string }) {
  return <>{text.split(/(`[^`]+`)/).map((p, i) => (p.length > 2 && p.startsWith('`') && p.endsWith('`') ? <code key={i} className="rounded bg-soft px-1 font-mono text-[0.92em]">{p.slice(1, -1)}</code> : p))}</>;
}

const rowClass = 'flex min-h-11 w-full cursor-pointer flex-col items-start justify-center gap-0.5 rounded-xl border border-line-strong bg-surface px-3 py-2 text-left text-ink hover:bg-soft';

const linkBtn = 'min-h-11 cursor-pointer self-start bg-transparent px-1 text-left text-[13px] text-muted underline-offset-2 hover:text-ink hover:underline';
const field = 'w-full rounded-lg border border-line-strong bg-surface p-2 text-sm text-ink';

// A note rides along with the option tapped, so every answer stays one tap.
function Options({ d, busy, send }: { d: Question; busy: boolean; send: (a: Answer) => void }) {
  const [mode, setMode] = useState<'' | 'note' | 'text'>('');
  const [value, setValue] = useState('');
  const pick = d.suggested ?? -1;
  const labels = pick > 0 && pick < d.options.length ? [d.options[pick]!, ...d.options.filter((_, i) => i !== pick)] : d.options;
  const note = mode === 'note' ? value.trim() : '';
  const choose = (o: string) => send(note ? { choice: o, note } : { choice: o });
  const toggle = (m: 'note' | 'text') => { setMode(mode === m ? '' : m); setValue(''); };
  return (
    <div className="flex flex-col gap-2">
      {mode === 'note' && <textarea data-answer aria-label="Note" placeholder="Your note goes with the option you tap" maxLength={2000} rows={2} value={value} onChange={(e) => setValue(e.target.value)} className={field} />}
      {labels.filter((o) => o !== YOU_DECIDE).map((o) => (
        <button key={o} type="button" disabled={busy} onClick={() => choose(o)} className={rowClass}>
          <span className="flex flex-wrap items-center gap-2 text-sm font-semibold [overflow-wrap:anywhere]">
            {o}
            {pick >= 0 && o === d.options[pick] && <span className="rounded-full bg-soft px-2 py-0.5 text-[11px] font-medium text-ink-2">Suggested</span>}
          </span>
          {d.optionDescriptions?.[o] && <span className="text-[13px] font-normal text-muted [overflow-wrap:anywhere]">{d.optionDescriptions[o]}</span>}
        </button>
      ))}
      <div className="flex flex-wrap gap-x-3">
        {labels.includes(YOU_DECIDE) && <button type="button" disabled={busy} onClick={() => choose(YOU_DECIDE)} className={linkBtn}>{YOU_DECIDE}</button>}
        {d.freeText && <button type="button" aria-expanded={mode === 'note'} onClick={() => toggle('note')} className={linkBtn}>{mode === 'note' ? 'Remove note' : 'Add a note'}</button>}
        {d.freeText && <button type="button" aria-expanded={mode === 'text'} onClick={() => toggle('text')} className={linkBtn}>Something else…</button>}
      </div>
      {mode === 'text' && <textarea data-answer aria-label="Your answer" maxLength={2000} rows={3} value={value} onChange={(e) => setValue(e.target.value)} className={field} />}
      {mode === 'text' && <button type="button" disabled={busy || !value.trim()} onClick={() => send({ text: value })} className="min-h-11 cursor-pointer self-start rounded-xl border border-line-strong bg-surface px-4 text-sm font-medium text-ink hover:bg-soft disabled:opacity-50">Send</button>}
    </div>
  );
}

function EvidenceFold({ d, rich }: { d: Question; rich: boolean }) {
  if (!d.evidence) return null;
  return (
    <details className="text-[13px] text-ink-2">
      <summary className="cursor-pointer">{rich ? 'Why this matters' : 'Evidence'}</summary>
      <pre className="m-0 mt-2 max-h-60 overflow-auto whitespace-pre-wrap rounded-lg bg-soft p-2.5 font-mono text-[12px] [overflow-wrap:anywhere]">{d.evidence}</pre>
    </details>
  );
}

/** A question an extension escalated to a person; each option is a button. */
export function QuestionCard({ sessionKey, d, now, title, done }: { sessionKey: string; d: Question; now: number; title?: string; done?: () => void }) {
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [answered, setAnswered] = useState<number>();
  const choose = (answer: Answer) => {
    if (busy) return;
    setBusy(true);
    setError('');
    void answerQuestion(sessionKey, d.id, answer).then(() => { setAnswered(d.askedAt); done?.(); }, (e: Error) => setError(e.message)).finally(() => setBusy(false));
  };
  const rich = !!(d.header || d.title || d.detail || d.suggested != null || d.freeText || Object.keys(d.optionDescriptions ?? {}).length);
  if (answered === d.askedAt) return null; // a re-ask has a newer askedAt
  return (
    <section id={needId(d.id)} aria-label="Question" className="flex flex-col gap-2.5 rounded-2xl border border-warning/40 bg-warm/60 p-3.5">
      <div className="flex items-center gap-2">
        <span className="text-[11px] font-bold tracking-[.06em] text-warning">NEEDS YOU</span>
        {d.header && <span className="rounded-full bg-soft px-2 py-0.5 text-[11px] font-semibold tracking-[.06em] text-ink-2 uppercase">{d.header}</span>}
        {d.askedAt > 0 && <time className="ml-auto text-xs tabular-nums text-muted">{age(new Date(d.askedAt).toISOString(), now)}</time>}
      </div>
      <p className="m-0 text-[14.5px] font-semibold [overflow-wrap:anywhere]">{d.title ? <Headline text={d.title} /> : d.question}</p>
      {d.detail && <div className="text-[13.5px] text-ink-2"><Markdown text={d.detail} /></div>}
      {title && <a href={`#/s/${encodeURIComponent(sessionKey)}`} className="line-clamp-2 text-[13px] text-link max-lg:-my-2 max-lg:py-3 [overflow-wrap:anywhere]">{title}</a>}
      <EvidenceFold d={d} rich={rich} />
      {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
      {rich ? <Options d={d} busy={busy} send={choose} /> : (
        <div className="grid grid-cols-[repeat(auto-fit,minmax(7.5rem,1fr))] gap-2">
          {d.options.map((o) => (
            <button key={o} type="button" disabled={busy} onClick={() => choose({ choice: o })} className="min-h-11 cursor-pointer rounded-xl border border-line-strong bg-surface px-3 text-sm font-medium text-ink [overflow-wrap:anywhere] hover:bg-soft">{o}</button>
          ))}
        </div>
      )}
    </section>
  );
}

/** The session view's open questions, as the session's socket reports them. */
export function SessionQuestions({ sessionKey, items }: { sessionKey: string; items: Question[] }) {
  // A waiting session emits no frames, so the age label needs its own tick.
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 15_000);
    return () => clearInterval(t);
  }, []);
  return <>{items.map((q) => <QuestionCard key={q.id} sessionKey={sessionKey} d={q} now={now} />)}</>;
}
