import { defineExtension, type Extension } from '@earendil-works/pi-durable';
import type { Action, ActionContext } from '@quack/sdk';
import { distributor } from './client.ts';
import { homeTools } from './tools.ts';

export { manifest } from './manifest.ts';

/** The home agent's tools over the distributor, with the actions the loaded extensions contribute. */
export const homeSessionExtensions = (actions: Action<any>[], ctx: ActionContext): Extension[] => [
  defineExtension({ name: 'home', tools: homeTools(distributor(ctx.distributorUrl, ctx.fetch), actions, ctx) as never }),
];
