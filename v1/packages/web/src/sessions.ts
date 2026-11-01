import type { Approval, Decision, IndexArtifact, SessionSummary } from './api.ts';
import { searchText, sessionTitle } from './kinds.ts';

export type Row = SessionSummary & { approvals: Approval[]; decisions: Decision[]; artifacts: IndexArtifact[]; multiRepo: boolean };
export type Groups = { needs: Row[]; running: Row[]; recent: Row[] };
export type Filters = { text: string; agent: string; start: string };

// The display title as one string, e.g. "#1927 Sleeper waivers job"; the full key stays in the row's tooltip.
export function titleOf(s: SessionSummary & { multiRepo?: boolean }): string {
  const t = sessionTitle(s);
  return [t.repo, t.number, t.text].filter(Boolean).join(' ');
}

const GITHUB_KEY = /^github:([^/]+)\/([^#]+)#(\d+)$/;

// GitHub redirects /pull/<n> to /issues/<n> for a plain issue, so one URL fits both.
export const githubUrl = (key: string) => {
  const m = GITHUB_KEY.exec(key);
  return m ? `https://github.com/${m[1]}/${m[2]}/pull/${m[3]}` : undefined;
};

const START_LABELS: Record<string, string> = { ui: 'You', github: 'GitHub' };

// The key prefix names what started the session (ui:, github:, ...).
export function startOf(key: string): string {
  const prefix = key.split(':')[0] ?? key;
  return START_LABELS[prefix] ?? prefix;
}

export function matches(s: Row, f: Filters): boolean {
  const q = f.text.trim().toLowerCase();
  const hit = !q || [...searchText(s), s.key, s.agent].some((v) => v.toLowerCase().includes(q));
  return hit && (!f.agent || s.agent === f.agent) && (!f.start || startOf(s.key) === f.start);
}

export function group(rows: Row[]): Groups {
  const needs = rows.filter((r) => r.approvals.length + r.decisions.length > 0);
  const rest = rows.filter((r) => r.approvals.length + r.decisions.length === 0);
  return { needs, running: rest.filter((r) => r.active), recent: rest.filter((r) => !r.active) };
}

export function statusLine(r: Row): string {
  if (r.approvals.length) return `Approve ${r.approvals[0]!.tool}${r.approvals.length > 1 ? ` +${r.approvals.length - 1} more` : ''}`;
  if (r.decisions.length) return `Answer: ${r.decisions[0]!.question}`;
  if (r.note) return r.note;
  return `${r.agent} · ${r.running ? 'running' : 'idle'}`;
}

export function age(iso: string, now: number): string {
  const min = Math.max(0, Math.floor((now - Date.parse(iso)) / 60_000));
  if (min < 1) return 'now';
  if (min < 60) return `${min}m`;
  if (min < 1440) return `${Math.floor(min / 60)}h`;
  return new Date(iso).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}

// A composer's first message waits here for the new session's socket to open.
export const pendingInput = new Map<string, string>();
