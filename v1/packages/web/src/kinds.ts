import type { IndexArtifact, SessionSummary } from './api.ts';
import { BLUE, plain, type Delivered, type SessionKind, type Tone, type Words } from './extension.ts';
import { EXTENSIONS } from './extensions/index.ts';
import { failedLine, type Row } from './sessions.ts';

export type { Tone };

export type Look = { delivered?: boolean; link?: Delivered['link']; label: string; icon: string; tint: string; repo?: string; num?: string; title: string; full: string; line: string; tone: Tone };

const DOT = 'M9 12a3 3 0 1 0 6 0a3 3 0 1 0-6 0';

// The newest sealed version of a kind for this session, from the index.
function newest(r: Row, kind: string): IndexArtifact | undefined {
  return r.artifacts.filter((a) => a.kind === kind).sort((a, b) => b.version - a.version || b.sealedAt - a.sealedAt)[0];
}


// A running session says what it does, a finished one says how its newest artifact fared.
function outcome(r: Row, kind: string, w: Words, delivered?: Delivered): Pick<Look, 'line' | 'tone' | 'link' | 'delivered'> {
  const a = newest(r, kind);
  const v = a?.verdict;
  if (r.active) return { line: w.run, tone: 'muted' };
  if (delivered) return { ...delivered, delivered: true };
  if (!a) return { line: w.none, tone: 'muted' };
  if (!v) return { line: w.sealed ?? 'Ready', tone: 'muted' };
  return v.passed ? { line: w.pass, tone: 'ok' } : { line: w.fail, tone: 'bad' };
}

const FALLBACK: SessionKind = { label: '', match: () => true, icon: DOT, tint: BLUE, name: (r) => plain(r.title ?? r.key), words: () => undefined };
const KINDS: SessionKind[] = [...EXTENSIONS.flatMap((e) => e.session ?? []), FALLBACK];

export function look(r: Row): Look {
  const kind = KINDS.find((x) => x.match(r))!;
  const { num, repo, title, full } = kind.name(r);
  const w = kind.words(r);
  const state = w ? outcome(r, w.kind, w.w, kind.delivered?.(r)) : { line: r.agent, tone: 'muted' as const };
  const ask = r.approvals[0];
  const more = r.approvals.length > 1 ? ` +${r.approvals.length - 1} more` : '';
  const question = r.questions[0];
  const failure = r.failures[0];
  const shown = failure ? { line: failedLine(failure), tone: 'bad' as const } : ask ? { line: `Approve ${ask.tool}${more}`, tone: 'warn' as const } : question ? { line: `Answer: ${question.question}`, tone: 'warn' as const } : state;
  return { label: kind.label, icon: kind.icon, tint: kind.tint, num, repo, title, full, ...shown };
}

// What search matches besides the key and agent: the title as shown, and the kind's name.
export const searchText = (r: Row): string[] => {
  const l = look(r);
  return [l.full, l.num ?? '', l.title, l.label];
};

export type SessionTitle = { repo?: string; number?: string; text: string; full: string; icon: string; kind: string };

// The one place a session is named: list rows, the session header, Inbox and Artifacts all read this.
export function sessionTitle(s: SessionSummary & { multiRepo?: boolean }): SessionTitle {
  const l = look({ multiRepo: false, ...s, approvals: [], questions: [], failures: [], artifacts: [] });
  return { repo: l.repo, number: l.num, text: l.title, full: l.full, icon: l.icon, kind: l.label };
}

/** How the Artifacts page names a session's artifacts: the kind's noun and the filter chip it falls under. */
export function artifactKind(r: Row | undefined): { noun: string; group: string } {
  const k = r && KINDS.find((x) => x.match(r));
  return { noun: (r && k?.noun?.(r)) || '', group: k?.group ?? 'Other' };
}
