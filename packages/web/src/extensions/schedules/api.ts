import { json } from '../../api.ts';

export type ScheduleItem = {
  id: string; agent: string; sessionKey: string; input: string; cron?: string; tz?: string; times?: string[];
  source: 'agent' | 'api'; lastRun: string | null; nextRun: string | null; missedRuns: string[];
};

// The page never holds the schedules service token: the distributor adds it (apps/schedules/*).
export const listSchedules = async () => (await json<{ schedules: ScheduleItem[] }>(await fetch('apps/schedules'))).schedules;
// The service refuses to delete a schedule an agent declares; those change with the agent.
export const deleteSchedule = async (id: string) => void (await json(await fetch(`apps/schedules/${encodeURIComponent(id)}`, { method: 'DELETE' })));
