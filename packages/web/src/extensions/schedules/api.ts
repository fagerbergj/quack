import { json, listAll } from '../../api.ts';

export type ScheduleItem = {
  id: string; agent: string; sessionKey: string; input: string; cron?: string; tz?: string; times?: string[];
  lastRun: string | null; nextRun: string | null; missedRuns: string[];
};

// The page never holds the schedules service token: the distributor adds it (apps/schedules/*).
export const listSchedules = () => listAll<ScheduleItem>('apps/schedules', 'schedules');
// A 204 has no body to parse. keepalive lets a DELETE sent as the tab closes outlive the document.
export const deleteSchedule = async (id: string) => {
  const res = await fetch(`apps/schedules/${encodeURIComponent(id)}`, { method: 'DELETE', keepalive: true });
  if (!res.ok) await json(res);
};
