import type { IndexArtifact } from './api.ts';
import type { Row } from './sessions.ts';

export type Tone = 'plain' | 'muted' | 'warn' | 'ok' | 'bad';
export type Look = { label: string; icon: string; tint: string; num?: string; title: string; line: string; tone: Tone };

// SVG path data per icon (24px grid); the PR icon's dots are part of its path.
const ICON = {
  pr: 'M3.5 6a2.5 2.5 0 1 0 5 0a2.5 2.5 0 1 0-5 0M15.5 18a2.5 2.5 0 1 0 5 0a2.5 2.5 0 1 0-5 0M6 8.5v11M18 15.5V9a3 3 0 0 0-3-3h-4M13 3.5 10.5 6 13 8.5',
  search: 'M18 11a7 7 0 1 0-14 0a7 7 0 1 0 14 0M20 20l-3.5-3.5',
  trophy: 'M8 4h8v5a4 4 0 0 1-8 0ZM8 6H5a3 3 0 0 0 3 4M16 6h3a3 3 0 0 1-3 4M12 13v4M9 20h6',
  pencil: 'M4 20h4L19 9l-4-4L4 16ZM13.5 6.5l4 4',
  dot: 'M9 12a3 3 0 1 0 6 0a3 3 0 1 0-6 0',
};
const BLUE = 'bg-link-bg text-link';

const GITHUB_KEY = /^github:[^/]+\/([^#]+)#(\d+)$/;
const SLEEPER_KEY = /^sleeper:[^:]+:(\d+):lineup$/;

// The newest sealed version of a kind for this session, from the index.
function newest(r: Row, kind: string): IndexArtifact | undefined {
  return r.artifacts.filter((a) => a.kind === kind).sort((a, b) => b.version - a.version)[0];
}

type Words = { run: string; pass: string; fail: string; none: string };

// A running session says what it does, a finished one says how its newest artifact fared.
function outcome(r: Row, kind: string, w: Words): Pick<Look, 'line' | 'tone'> {
  const v = newest(r, kind)?.verdict;
  if (r.running) return { line: w.run, tone: 'muted' };
  if (!v) return { line: w.none, tone: 'muted' };
  return v.passed ? { line: w.pass, tone: 'ok' } : { line: w.fail, tone: 'bad' };
}

function remarkableTitle(r: Row): string {
  return r.title ?? r.key.slice('remarkable:'.length);
}

type Kind = { label: string; match: (r: Row) => boolean; look: (r: Row) => Omit<Look, 'label' | 'line' | 'tone'> & Partial<Pick<Look, 'line' | 'tone'>> };

// A new extension adds one entry here; the first match wins and the last one catches everything.
const KINDS: Kind[] = [
  {
    label: 'review', match: (r) => GITHUB_KEY.test(r.key),
    look: (r) => {
      const m = GITHUB_KEY.exec(r.key)!;
      const words = { run: 'Reviewing', pass: 'Approved ✓', fail: 'Checks failed', none: 'Not reviewed yet' };
      return { icon: ICON.pr, tint: BLUE, num: `#${m[2]}`, title: r.title ?? m[1]!, ...outcome(r, 'github.review', words) };
    },
  },
  {
    label: 'research', match: (r) => r.agent === 'researcher' || r.key.startsWith('research:'),
    look: (r) => ({ icon: ICON.search, tint: BLUE, title: r.title ?? r.key, ...outcome(r, 'research.report', { run: 'Researching', pass: 'Report ✓', fail: 'Report ✗', none: 'No report yet' }) }),
  },
  {
    label: 'lineup', match: (r) => SLEEPER_KEY.test(r.key),
    look: (r) => ({ icon: ICON.trophy, tint: 'bg-success-bg text-success', title: `Lineup · week ${SLEEPER_KEY.exec(r.key)![1]}`, ...outcome(r, 'sleeper.cards', { run: 'Checking lineup', pass: 'Card ready', fail: 'Checks failed', none: 'No card yet' }) }),
  },
  {
    label: 'reMarkable', match: (r) => r.key.startsWith('remarkable:'),
    look: (r) => ({ icon: ICON.pencil, tint: BLUE, title: remarkableTitle(r), ...outcome(r, 'remarkable.transcript', { run: 'Transcribing', pass: 'Transcribed ✓', fail: 'Failed: checks did not pass', none: 'Not transcribed yet' }) }),
  },
  { label: '', match: () => true, look: (r) => ({ icon: ICON.dot, tint: BLUE, title: r.title ?? r.key, line: r.agent, tone: 'muted' }) },
];

export function look(r: Row): Look {
  const k = KINDS.find((kind) => kind.match(r))!;
  const { line, tone, ...rest } = k.look(r);
  const more = r.approvals.length > 1 ? ` +${r.approvals.length - 1} more` : '';
  const ask = r.approvals[0];
  return { label: k.label, ...rest, ...(ask ? { line: `Approve ${ask.tool}${more}`, tone: 'warn' as const } : { line: line ?? '', tone: tone ?? 'plain' }) };
}

// What search matches besides the key and agent: the title as shown, and the kind's name.
export const searchText = (r: Row): string[] => {
  const l = look(r);
  return [l.num ? `${l.num} ${l.title}` : l.title, l.label];
};
