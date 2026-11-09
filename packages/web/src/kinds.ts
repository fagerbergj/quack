import { BLUE, plain, type Nameable, type SessionKind, type Tone, type Words } from './extension.ts';
import { EXTENSIONS } from './extensions/index.ts';
import { failedLine, type Row } from './sessions.ts';

export type { Tone };

export type Look = { label: string; icon: string; tint: string; repo?: string; num?: string; title: string; full: string; line: string; tone: Tone };

const CHAT = 'M5 5h14a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2h-7l-5 4v-4H5a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2zM8 10h8M8 13h5';
const DOC = 'M7 3h7l5 5v13H7zM14 3v5h5M10 13h6M10 17h6';

const ENDED: Record<string, string> = { aborted: 'Stopped', denied: 'Stopped at a rejected approval', failed: 'Run failed' };

// A running session says what it does, an idle one how its last run ended.
function state(r: Row, w: Words): Pick<Look, 'line' | 'tone'> {
  if (r.state === 'running') return { line: w.run, tone: 'muted' };
  const ended = r.lastRun?.outcome;
  return { line: !ended ? w.none : ended === 'succeeded' ? w.done : ENDED[ended]!, tone: 'muted' };
}

const FALLBACK: SessionKind = { label: '', match: () => true, icon: DOC, tint: BLUE, name: (r) => plain(r.title ?? r.key), words: () => undefined };
// Conversations started from Home; the row says Chat or Working, never the agent's name.
const HOME: SessionKind = { label: 'Chat', match: (r) => r.key.startsWith('home:'), icon: CHAT, tint: BLUE, name: (r) => plain(r.title ?? 'Chat with quack'), words: () => ({ run: 'Working', done: 'Chat', none: 'Chat' }) };
const KINDS: SessionKind[] = [...EXTENSIONS.flatMap((e) => e.session ?? []), HOME, FALLBACK];
const kindOf = (r: Nameable) => KINDS.find((x) => x.match(r))!;

const waiting = (n: number) => (n > 1 ? `${n} waiting on you` : 'Waiting on you');

export function look(r: Row): Look {
  const kind = kindOf(r);
  const { num, repo, title, full } = kind.name(r);
  const w = kind.words(r);
  const failure = r.failures[0];
  const shown = failure ? { line: failedLine(failure), tone: 'bad' as const } : r.pendingAsks ? { line: waiting(r.pendingAsks), tone: 'warn' as const } : w ? state(r, w) : { line: r.agent, tone: 'muted' as const };
  return { label: kind.label, icon: kind.icon, tint: kind.tint, num, repo, title, full, ...shown };
}

export type SessionTitle = { repo?: string; number?: string; text: string; full: string; icon: string; kind: string };

// The one place a session is named: list rows, the session header, Home and Artifacts all read this.
export function sessionTitle(s: Nameable): SessionTitle {
  const kind = kindOf(s);
  const n = kind.name(s);
  return { repo: n.repo, number: n.num, text: n.title, full: n.full, icon: kind.icon, kind: kind.label };
}

/** How the Artifacts page names a session's artifacts: the kind's noun. */
export const artifactKind = (r: Row | undefined): { noun: string } => ({ noun: (r && kindOf(r).noun?.(r)) || '' });
