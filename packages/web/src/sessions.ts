import { escapeTerm } from '@quack/sdk/query';
import { spendOf, type Failure, type Session, type Spend } from './api.ts';
import type { Img } from './Attach.tsx';
import { EXTENSIONS } from './extensions/index.ts';
import { sessionTitle } from './kinds.ts';
import { searchTerms, type FilterValues } from './SearchFilter.tsx';

/** A session as the lists show it; `failures` are its last run's failed steps, empty while a new run works on them. */
export type Row = Session & { failures: Failure[]; multiRepo: boolean; spend: Spend };

// A failure is shown only while it is the latest thing: activity or an ask after it (a run waiting on approval) supersedes it.
const latestFailure = ({ lastRun: r, lastActivity, oldestAskAt }: Session) =>
  r?.outcome === 'failed' && ![lastActivity, oldestAskAt].some((t) => t !== undefined && Date.parse(t) > Date.parse(r.endedAt));

export const rowOf = (s: Session, multiRepo: boolean): Row =>
  ({ ...s, multiRepo, spend: spendOf(s), failures: s.state !== 'running' && latestFailure(s) ? s.lastRun!.failedSteps : [] });
export type Groups = { needs: Row[]; running: Row[]; recent: Row[] };

// The display title as one string, e.g. "#1927 Add retries"; the full key stays in the row's tooltip.
export function titleOf(s: Parameters<typeof sessionTitle>[0]): string {
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

/** The Sessions list's q: `start` is a key prefix, and the default view leaves archived sessions out. */
export function sessionQuery(text: string, f: FilterValues): string {
  return [
    f.view === 'archived' ? 'state:archived' : 'NOT state:archived',
    ...(f.agent ? [`agent:${escapeTerm(f.agent)}`] : []),
    ...(f.start ? [`key:${escapeTerm(`${f.start}:`)}*`] : []),
    ...searchTerms(text),
  ].join(' AND ');
}

const asks = (r: Row) => r.pendingAsks > 0 || r.failures.length > 0;

export function group(rows: Row[]): Groups {
  const needs = rows.filter(asks);
  const rest = rows.filter((r) => !asks(r));
  return { needs, running: rest.filter((r) => r.state === 'running'), recent: rest.filter((r) => r.state !== 'running') };
}

/** The row's line for a failed run: the step and the error's first line. */
export const failedLine = (f: Failure) => `Run failed at ${f.step} · ${f.error.split('\n')[0]}`;

/** The anchor an approval or question card carries in the ledger, which the context rail scrolls to. */
export const needId = (id: string) => `need-${id}`;

// 2417000 reads as 2.4M, not 2417k; one decimal only below ten.
/** Whole seconds as "42 s", "12 min 36 s" or "1 h 5 min": the two largest units, zero parts dropped. */
export function duration(seconds: number) {
  const s = Math.round(seconds);
  if (s < 60) return `${s} s`;
  const [h, m, r] = [Math.floor(s / 3600), Math.floor((s % 3600) / 60), s % 60];
  const part = (n: number, u: string) => (n ? ` ${n} ${u}` : '');
  return (h ? `${h} h${part(m, 'min')}` : `${m} min${part(r, 's')}`);
}

export const compact = (n: number) => (n >= 1e6 ? `${+(n / 1e6).toFixed(n < 1e7 ? 1 : 0)}M` : n >= 1000 ? `${Math.round(n / 1000)}k` : String(n));

/** The short spend reading (dollars when the model is priced, else input plus output tokens) and its token breakdown for the tooltip; undefined when nothing was spent. */
export function spendLabel(s: Spend | undefined): { text: string; title: string } | undefined {
  if (!s) return undefined;
  const { input, output, cacheRead, cacheWrite } = s.tokens;
  const used = input + output;
  if (s.cost <= 0 && used === 0) return undefined;
  const text = s.cost <= 0 ? `${compact(used)} tokens` : s.cost < 0.01 ? '<$0.01' : `$${s.cost.toFixed(2)}`;
  return { text, title: [`${COUNT.format(input)} in`, `${COUNT.format(output)} out`, `${COUNT.format(cacheRead)} cached`, ...(cacheWrite ? [`${COUNT.format(cacheWrite)} cache write`] : [])].join(' · ') };
}

const DAY = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric' });
const COUNT = new Intl.NumberFormat();

export function age(iso: string, now: number): string {
  const min = Math.max(0, Math.floor((now - Date.parse(iso)) / 60_000));
  if (min < 1) return 'now';
  if (min < 60) return `${min}m`;
  if (min < 1440) return `${Math.floor(min / 60)}h`;
  return DAY.format(new Date(iso));
}

// A composer's first message waits here for the new session's socket to open.
export const pendingInput = new Map<string, { text: string; images: Img[] }>();
