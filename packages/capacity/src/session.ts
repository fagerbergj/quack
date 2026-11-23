import { randomUUID } from 'node:crypto';
import { AgentDoc, defineExtension, GenerationTask, hook, type Extension } from '@earendil-works/pi-durable';
import { appEndpoint, gateGenerations, modelWait, onRunEnded, onWaiting } from '@quack/sdk';
import { CapacityGate, httpClient, type GateOptions, type GatedCall } from './gate.ts';

export type CapacityConfig = Partial<Pick<GateOptions, 'pollMs'>> & { serviceUrl?: string; serviceToken?: string; holder?: string };

/** The deployment's gate: installed for every session the runtime primed with a distributor to reach, otherwise nothing. */
export function capacityFromEnv(env: Record<string, string | undefined> = process.env) {
  const app = reachable();
  return capacitySessionExtensions({ serviceUrl: app?.url, serviceToken: app?.token, holder: env.QUACK_SESSION_KEY });
}

// An unprimed endpoint is a session launched without a distributor, which runs ungated.
function reachable() {
  try {
    return appEndpoint('capacity');
  } catch {
    return undefined;
  }
}

/** The session half: a lease on the model endpoint per run, taken before a request and kept until the run ends or waits on a person or a child run. Without a service it installs nothing. */
export function capacitySessionExtensions(config: CapacityConfig, gate?: CapacityGate): Extension[] {
  const holder = config.holder ?? randomUUID();
  if (!gate && !(config.serviceUrl && config.serviceToken)) return [];
  const g = gate ?? new CapacityGate({ ...config, holder, client: httpClient(config.serviceUrl!, config.serviceToken!), setWait: modelWait.set });
  onWaiting((n) => n > 0 && void g.idle());
  // Direct completions (the judge, workflow model steps) bypass GenerationTask, so they take the same lease.
  gateGenerations({ before: (provider, signal, shared) => g.begin(provider, signal, shared), after: (call) => g.end(call as GatedCall) });
  // A generation's lease is paired with its task, so calls finishing out of order free their own.
  const calls = new Map<string, GatedCall>();
  // The run's end gives every lease back, the one kept between generations included.
  onRunEnded(() => (calls.clear(), void g.release()));
  return [defineExtension({
    name: 'capacity',
    hooks: [hook(GenerationTask, {
      // The lease is keyed by the agent's provider: one provider is one model endpoint.
      async beforeRequest(_request, api, ctx) {
        const endpoint = (await api.snapshot(AgentDoc, api.conversationId, ctx))?.model?.provider;
        if (!endpoint) return undefined;
        const key = String(api.taskId);
        const stale = calls.get(key);
        if (stale) g.end(stale);
        const call = await g.begin(endpoint, ctx.abortSignal);
        calls.set(key, call);
        // The signal aborts when the invocation ends, also when an abort throws before afterResponse, so the call ends with it.
        const close = () => (calls.get(key) === call && calls.delete(key), g.end(call));
        if (ctx.abortSignal?.aborted) close();
        else ctx.abortSignal?.addEventListener('abort', close, { once: true });
        return undefined;
      },
      afterResponse: (_message, api) => {
        const call = calls.get(String(api.taskId));
        calls.delete(String(api.taskId));
        if (call) g.end(call);
      },
    })],
  })];
}
