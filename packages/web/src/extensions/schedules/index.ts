import type { Extension } from '../../extension.ts';
import { Schedules } from './Schedules.tsx';

export const schedules: Extension = {
  nav: { route: 'schedules', label: 'Schedules', icon: 'M12 7v5l3 2M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18', tint: 'bg-soft text-ink-2', page: Schedules },
};
