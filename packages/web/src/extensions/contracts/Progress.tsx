import type { Ask } from '../../api.ts';
import { Markdown } from '../../Markdown.tsx';
import { useLoad } from '../../useLoad.ts';
import { card, label } from './RestView.tsx';

/** This session's answered questions, oldest first, from the Sessions API's asks; none when the read fails. */
export async function decidedQuestions(key: string): Promise<Ask[]> {
  const q = new URLSearchParams({ pageSize: '1000', q: 'type:question state:resolved' });
  const res = await fetch(`sessions/${encodeURIComponent(key)}/asks?${q}`).catch(() => undefined);
  if (!res?.ok) return [];
  const asks = ((await res.json().catch(() => ({}))) as { asks?: Ask[] }).asks ?? [];
  return asks.sort((a, b) => a.createdAt.localeCompare(b.createdAt));
}

const NONE: Ask[] = [];
const titleOf = (a: Ask) => a.prompt?.title || a.prompt?.question || 'Question';

/** The questions the person already answered, the design decisions behind the contract; read again whenever one is answered or a run ends. */
export function Decided({ sessionKey, asked, running, open }: { sessionKey: string; asked: number; running: boolean; open: boolean }) {
  const { data } = useLoad(() => decidedQuestions(sessionKey), [sessionKey, asked, running], NONE);
  if (!data.length) return null;
  return (
    <details open={open} className={`${card} group px-4 py-3`}>
      <summary className={`min-h-11 cursor-pointer content-center lg:min-h-8 ${label}`}>{data.length} {data.length === 1 ? 'decision' : 'decisions'} so far</summary>
      <ul className="m-0 mt-1 flex list-none flex-col gap-2 p-0 text-[13.5px]">
        {data.map((a) => (
          <li key={a.id} className="[overflow-wrap:anywhere]">
            <span className="text-muted">Decided: </span>{titleOf(a)} <span aria-hidden="true" className="text-muted">→</span><span className="sr-only">:</span> <strong className="font-semibold">{a.resolution?.text || a.resolution?.choice}</strong>
            {a.resolution?.reason && <span className="block text-[12.5px] text-muted">{a.resolution.reason}</span>}
          </li>
        ))}
      </ul>
    </details>
  );
}

/** Before the first version: what the run is doing now and the brief it works from. */
export function Progress({ activity, brief, waiting }: { activity: string; brief?: string; waiting: boolean }) {
  return (
    <section aria-label="Progress" className={`${card} flex flex-col gap-2 px-4 py-3`}>
      <p role="status" className="m-0 flex items-center gap-2 text-[14px] font-semibold"><span aria-hidden="true" className={`h-2 w-2 flex-none rounded-full ${waiting ? 'bg-warning' : 'bg-accent'}`} />{activity}</p>
      <p className="m-0 text-[13px] text-muted">No version yet: the contract shows here as soon as the agent writes its first one.</p>
      {brief && (
        <details className="text-[13.5px]">
          <summary className="min-h-11 cursor-pointer content-center text-ink-2 lg:min-h-8">Brief</summary>
          <div className="mt-1 text-ink-2"><Markdown text={brief} /></div>
        </details>
      )}
    </section>
  );
}
