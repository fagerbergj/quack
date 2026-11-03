import type { Approval, Failure, Question, IndexArtifact, SessionSummary } from './api.ts';
import { EXTENSIONS } from './extensions/index.ts';
import { searchText, sessionTitle } from './kinds.ts';

export type Row = SessionSummary & { approvals: Approval[]; questions: Question[]; failures: Failure[]; artifacts: IndexArtifact[]; multiRepo: boolean };
export type Groups = { needs: Row[]; running: Row[]; recent: Row[] };
export type Filters = { text: string; agent: string; start: string; view: '' | 'archived' };

// The display title as one string, e.g. "#1927 Add retries"; the full key stays in the row's tooltip.
export function titleOf(s: SessionSummary & { multiRepo?: boolean }): string {
  const t = sessionTitle(s);
  return [t.repo, t.number, t.text].filter(Boolean).join(' ');
}

/** An outside page for the session, if its extension has one. */
export const externalLink = (key: string) => EXTENSIONS.flatMap((e) => e.link?.(key) ?? [])[0];

// Reviews from one repo need no repo prefix; it earns its space once sessions span several scopes.
export const multiScope = (keys: string[]) => new Set(keys.flatMap((k) => EXTENSIONS.flatMap((e) => e.scope?.(k) ?? []))).size > 1;

// The key prefix names what started the session (ui:, then each extension's own).
export function startOf(key: string): string {
  const prefix = key.split(':')[0] ?? key;
  return prefix === 'ui' ? 'You' : (EXTENSIONS.find((e) => e.start?.prefix === prefix)?.start?.label ?? prefix);
}

export function matches(s: Row, f: Filters): boolean {
  const q = f.text.trim().toLowerCase();
  const hit = !q || [...searchText(s), s.key, s.agent].some((v) => v.toLowerCase().includes(q));
  return hit && Boolean(s.archived) === (f.view === 'archived') && (!f.agent || s.agent === f.agent) && (!f.start || startOf(s.key) === f.start);
}

const asks = (r: Row) => r.approvals.length + r.questions.length + r.failures.length > 0;

export function group(rows: Row[]): Groups {
  const needs = rows.filter(asks);
  const rest = rows.filter((r) => !asks(r));
  return { needs, running: rest.filter((r) => r.active), recent: rest.filter((r) => !r.active) };
}

/** The row's line for a failed run: the step and the error's first line. */
export const failedLine = (f: Failure) => `Run failed at ${f.step} · ${f.error.split('\n')[0]}`;

export function age(iso: string, now: number): string {
  const min = Math.max(0, Math.floor((now - Date.parse(iso)) / 60_000));
  if (min < 1) return 'now';
  if (min < 60) return `${min}m`;
  if (min < 1440) return `${Math.floor(min / 60)}h`;
  return new Date(iso).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}

// A composer's first message waits here for the new session's socket to open.
export const pendingInput = new Map<string, string>();
