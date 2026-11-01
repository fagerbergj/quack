import { CronExpressionParser } from 'cron-parser';

export type Schedule = {
  id: string;
  /** Agent the session is bound to on its first request; omitted uses the deployment's default. */
  agent?: string;
  /** Either a cron expression (UTC) or explicit ISO times. */
  cron?: string;
  times?: string[];
  /** `{time}` becomes the occurrence's ISO time, so each occurrence can have its own session. */
  sessionKey: string;
  input: string;
  /** Usefulness window (SCH-4): an occurrence stays useful for staleMs, and never past the deadline. Default: until the next occurrence. */
  staleMs?: number;
  deadline?: string;
};

const MAX_BACKLOG = 1000;

/** Rejects a definition the timer could not honor; returns it unchanged. */
export function validate(s: Schedule): Schedule {
  if (typeof s.id !== 'string' || !s.id || typeof s.sessionKey !== 'string' || !s.sessionKey || typeof s.input !== 'string') throw new Error('need id, sessionKey and input');
  if (!s.cron === !s.times) throw new Error('need exactly one of cron or times');
  if (s.times && (!Array.isArray(s.times) || s.times.some((t) => Number.isNaN(Date.parse(t))))) throw new Error('times must be ISO times');
  if (s.deadline !== undefined && Number.isNaN(Date.parse(s.deadline))) throw new Error('deadline must be an ISO time');
  if (s.staleMs !== undefined && !(s.staleMs > 0)) throw new Error('staleMs must be positive');
  if (s.cron) CronExpressionParser.parse(s.cron, { tz: 'UTC' });
  return s;
}

const sorted = (s: Schedule) => (s.times ?? []).map((t) => new Date(t)).sort((a, b) => +a - +b);

/** First occurrence strictly after `from`; undefined when explicit times run out. */
export function nextAfter(s: Schedule, from: Date): Date | undefined {
  if (s.times) return sorted(s).find((t) => t > from);
  return CronExpressionParser.parse(s.cron!, { currentDate: from, tz: 'UTC' }).next().toDate();
}

/** Occurrences in (after, upTo], newest first; a backlog past MAX_BACKLOG drops its oldest. */
export function between(s: Schedule, after: Date, upTo: Date): Date[] {
  if (s.times) return sorted(s).filter((t) => t > after && t <= upTo).reverse().slice(0, MAX_BACKLOG);
  const it = CronExpressionParser.parse(s.cron!, { currentDate: new Date(+upTo + 1), tz: 'UTC' });
  const found: Date[] = [];
  while (found.length < MAX_BACKLOG) {
    const t = it.prev().toDate();
    if (t <= after) break;
    found.push(t);
  }
  return found;
}

/** When the occurrence stops being worth running (ms since epoch). */
export function expiry(s: Schedule, occurrence: Date): number {
  const limits = [s.deadline && Date.parse(s.deadline), s.staleMs && +occurrence + s.staleMs].filter((n): n is number => typeof n === 'number');
  if (limits.length) return Math.min(...limits);
  return Number(nextAfter(s, occurrence) ?? Infinity);
}
