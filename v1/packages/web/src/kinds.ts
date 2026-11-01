import type { IndexArtifact, SessionSummary } from './api.ts';
import type { Row } from './sessions.ts';

export type Tone = 'plain' | 'muted' | 'warn' | 'ok' | 'bad';
export type Look = { label: string; icon: string; tint: string; repo?: string; num?: string; title: string; full: string; line: string; tone: Tone };

// SVG path data per icon (24px grid); the PR icon's dots are part of its path.
const ICON = {
  pr: 'M3.5 6a2.5 2.5 0 1 0 5 0a2.5 2.5 0 1 0-5 0M15.5 18a2.5 2.5 0 1 0 5 0a2.5 2.5 0 1 0-5 0M6 8.5v11M18 15.5V9a3 3 0 0 0-3-3h-4M13 3.5 10.5 6 13 8.5',
  search: 'M18 11a7 7 0 1 0-14 0a7 7 0 1 0 14 0M20 20l-3.5-3.5',
  trophy: 'M8 4h8v5a4 4 0 0 1-8 0ZM8 6H5a3 3 0 0 0 3 4M16 6h3a3 3 0 0 1-3 4M12 13v4M9 20h6',
  pencil: 'M4 20h4L19 9l-4-4L4 16ZM13.5 6.5l4 4',
  dot: 'M9 12a3 3 0 1 0 6 0a3 3 0 1 0-6 0',
};
const BLUE = 'bg-link-bg text-link';

const GITHUB_KEY = /^github:([^/]+)\/([^#]+)#(\d+)$/;
const HOME_REPO = 'fagerbergj/quack';
const MAX = 40;
// Sessions opened before titles were recorded carry the trigger prompt as their title; the PR's own title follows "Title: ".
const TRIGGER = /^Review pull request .*? Title: (.+)$/s;
const SLEEPER_KEY = /^sleeper:[^:]+:(\d+):(lineup|waivers)$/;
const CC_PREFIX = /^\w+(\([^)]*\))?!?:\s*/;

// The newest sealed version of a kind for this session, from the index.
function newest(r: Row, kind: string): IndexArtifact | undefined {
  return r.artifacts.filter((a) => a.kind === kind).sort((a, b) => b.version - a.version)[0];
}


// Cuts at a word boundary within MAX characters; short text passes through.
function cut(s: string): string {
  const t = s.replace(/\s+/g, ' ').trim();
  if (t.length <= MAX) return t;
  const head = t.slice(0, MAX);
  const at = head.lastIndexOf(' ');
  return `${(at > MAX / 2 ? head.slice(0, at) : head).replace(/[\s,.;:(-]+$/, '')}…`;
}

const sentence = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

// The question up to its first ?, , or . within MAX characters, else a word-boundary cut.
function question(s: string): string {
  const i = s.slice(0, MAX).search(/[?,.]/);
  return i < 0 ? cut(s) : s.slice(0, i + (s[i] === '?' ? 1 : 0)).trim();
}

// Sessions opened before titles were recorded carry the trigger prompt as their title.
function prTitle(r: Row, fallback: string): string {
  const t = (r.title && TRIGGER.exec(r.title)?.[1]) || (r.title?.startsWith('Review pull request ') ? '' : r.title) || '';
  return t.replace(CC_PREFIX, '') ? sentence(t.replace(CC_PREFIX, '')) : fallback;
}

type Words = { run: string; pass: string; fail: string; none: string };

// A running session says what it does, a finished one says how its newest artifact fared.
function outcome(r: Row, kind: string, w: Words): Pick<Look, 'line' | 'tone'> {
  const v = newest(r, kind)?.verdict;
  if (r.active) return { line: w.run, tone: 'muted' };
  if (!v) return { line: w.none, tone: 'muted' };
  return v.passed ? { line: w.pass, tone: 'ok' } : { line: w.fail, tone: 'bad' };
}

type Named = { title: string; full: string; num?: string; repo?: string };
type Kind = { label: string; match: (r: Row) => boolean; icon: string; tint: string; name: (r: Row) => Named; words: (r: Row) => { kind: string; w: Words } | undefined };

const plain = (full: string, text = cut(full)): Named => ({ title: text, full });

// A new extension adds one entry here; the first match wins and the last one catches everything.
const KINDS: Kind[] = [
  {
    label: 'review', match: (r) => GITHUB_KEY.test(r.key), icon: ICON.pr, tint: BLUE,
    name: (r) => {
      const [, owner, repo, n] = GITHUB_KEY.exec(r.key)!;
      const full = prTitle(r, repo!);
      return { title: cut(full), full, num: `#${n}`, ...(`${owner}/${repo}` !== HOME_REPO && { repo: `${owner}/${repo}` }) };
    },
    words: () => ({ kind: 'github.review', w: { run: 'Reviewing', pass: 'Approved ✓', fail: 'Checks failed', none: 'Not reviewed yet' } }),
  },
  {
    label: 'research', match: (r) => r.agent === 'researcher' || r.key.startsWith('research:'), icon: ICON.search, tint: BLUE,
    name: (r) => plain(r.title ?? r.key, question(r.title ?? r.key)),
    words: () => ({ kind: 'research.report', w: { run: 'Researching', pass: 'Report ✓', fail: 'Report ✗', none: 'No report yet' } }),
  },
  {
    label: 'sleeper', match: (r) => SLEEPER_KEY.test(r.key), icon: ICON.trophy, tint: 'bg-success-bg text-success',
    name: (r) => {
      const [, week, job] = SLEEPER_KEY.exec(r.key)!;
      return plain(`${sentence(job!)} · wk ${week}`);
    },
    words: () => ({ kind: 'sleeper.cards', w: { run: 'Checking lineup', pass: 'Card ready', fail: 'Checks failed', none: 'No card yet' } }),
  },
  {
    label: 'reMarkable', match: (r) => r.key.startsWith('remarkable:'), icon: ICON.pencil, tint: BLUE,
    name: (r) => plain(r.title ?? r.key.slice('remarkable:'.length)),
    words: () => ({ kind: 'remarkable.transcript', w: { run: 'Transcribing', pass: 'Transcribed ✓', fail: 'Failed: checks did not pass', none: 'Not transcribed yet' } }),
  },
  { label: '', match: () => true, icon: ICON.dot, tint: BLUE, name: (r) => plain(r.title ?? r.key), words: () => undefined },
];

export function look(r: Row): Look {
  const kind = KINDS.find((x) => x.match(r))!;
  const { num, repo, title, full } = kind.name(r);
  const w = kind.words(r);
  const state = w ? outcome(r, w.kind, w.w) : { line: r.agent, tone: 'muted' as const };
  const ask = r.approvals[0];
  const more = r.approvals.length > 1 ? ` +${r.approvals.length - 1} more` : '';
  const shown = ask ? { line: `Approve ${ask.tool}${more}`, tone: 'warn' as const } : state;
  return { label: kind.label, icon: kind.icon, tint: kind.tint, num, repo, title, full, ...shown };
}

// What search matches besides the key and agent: the title as shown, and the kind's name.
export const searchText = (r: Row): string[] => {
  const l = look(r);
  return [l.full, l.num ?? '', l.title, l.label];
};

export type SessionTitle = { repo?: string; number?: string; text: string; full: string; icon: string; kind: string };

// The one place a session is named: list rows, the session header, Inbox and Artifacts all read this.
export function sessionTitle(s: SessionSummary): SessionTitle {
  const l = look({ ...s, approvals: [], artifacts: [] });
  return { repo: l.repo, number: l.num, text: l.title, full: l.full, icon: l.icon, kind: l.label };
}
