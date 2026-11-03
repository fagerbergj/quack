import type { Extension } from '../extension.ts';
import { contracts } from './contracts/index.ts';
import { github } from './github.ts';
import { remarkable } from './remarkable/index.ts';
import { research } from './research.ts';
import { schedules } from './schedules/index.ts';
import { sleeper } from './sleeper/index.ts';

// A new extension adds one import and one entry; session kinds match in this order.
export const EXTENSIONS: Extension[] = [github, research, sleeper, remarkable, schedules, contracts];
