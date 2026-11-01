import { randomUUID } from 'node:crypto';
import { AgentDoc, defineExtension, GenerationTask, hook, type Extension } from '@earendil-works/pi-durable';
import { onWaiting, statusNote } from '@quack/sdk';
import { httpClient, SlotGate, type GateOptions } from './gate.ts';

export type SlotsConfig = Partial<Pick<GateOptions, 'graceMs' | 'capMs' | 'leaseTtlMs' | 'pollMs'>> & { serviceUrl?: string; serviceToken?: string; holder?: string };

/** The deployment's gate: installed for every session when QUACK_SLOTS_URL and QUACK_SLOTS_TOKEN are set, otherwise nothing. */
export const slotsFromEnv = (env: Record<string, string | undefined> = process.env) =>
  slotsSessionExtensions({ serviceUrl: env.QUACK_SLOTS_URL, serviceToken: env.QUACK_SLOTS_TOKEN, holder: env.QUACK_SESSION_KEY });

/** The session half: a model slot per run, taken before a request and kept between the run's turns. Without a service it installs nothing. */
export function slotsSessionExtensions(config: SlotsConfig, gate?: SlotGate): Extension[] {
  const holder = config.holder ?? randomUUID();
  if (!gate && !(config.serviceUrl && config.serviceToken)) return [];
  const g = gate ?? new SlotGate({ ...config, holder, client: httpClient(config.serviceUrl!, config.serviceToken!), setNote: statusNote.set });
  onWaiting((n) => n > 0 && void g.idle());
  return [defineExtension({
    name: 'slots',
    hooks: [hook(GenerationTask, {
      // The slot is keyed by the agent's provider: one provider is one model endpoint.
      async beforeRequest(_request, api, ctx) {
        const endpoint = (await api.snapshot(AgentDoc, api.conversationId, ctx))?.model?.provider;
        if (endpoint) await g.begin(endpoint, ctx.abortSignal);
        return undefined;
      },
      afterResponse: () => g.end(),
      onYield: async () => (await g.idle(), undefined),
    })],
  })];
}
