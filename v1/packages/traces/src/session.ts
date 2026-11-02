import { readFileSync } from 'node:fs';
import { defineExtension, GenerationTask, hook, type Extension } from '@earendil-works/pi-durable';

export type TracesConfig = { baseUrl?: string; publicKey?: string; secretKey?: string; sessionKey?: string; agentName?: string; agentVersion?: string };

type Event = { id: string; type: string; timestamp: string; body: Record<string, unknown> };

const MAX_QUEUE = 200;
const BATCH = 20;
const SEND_TIMEOUT_MS = 5_000;
const MAX_PENDING = 32;

/** afterResponse never fires on a provider error, so unmatched requests are capped, oldest first. */
export function remember<V>(map: Map<number, V>, key: number, value: V) {
  if (map.size >= MAX_PENDING) map.delete(map.keys().next().value!);
  map.set(key, value);
}

/** Fire-and-forget batches to Langfuse's public ingestion API; a failed batch is dropped, the oldest events go first when the queue is full. */
export class Sender {
  #queue: Event[] = [];
  #running = false;
  #url: string;
  #auth: string;
  constructor(url: string, auth: string) {
    this.#url = url;
    this.#auth = auth;
  }

  push(events: Event[]) {
    this.#queue.push(...events);
    this.#queue.splice(0, this.#queue.length - MAX_QUEUE);
    if (!this.#running) void this.#drain();
  }

  async #drain() {
    this.#running = true;
    while (this.#queue.length) {
      const batch = this.#queue.splice(0, BATCH);
      try {
        await fetch(this.#url, { method: 'POST', headers: { authorization: this.#auth, 'content-type': 'application/json' }, body: JSON.stringify({ batch }), signal: AbortSignal.timeout(SEND_TIMEOUT_MS) });
      } catch {
        // Tracing is best effort; an outage must not touch the generation.
      }
    }
    this.#running = false;
  }
}

function agentOf(path?: string) {
  try {
    const { name, version } = JSON.parse(readFileSync(path!, 'utf8'));
    return { agentName: name, agentVersion: version === undefined ? undefined : String(version) };
  } catch {
    return {};
  }
}

/** The deployment's trace sink: installed for every session when the three LANGFUSE_* variables are set, otherwise nothing. */
export const tracesFromEnv = (env: Record<string, string | undefined> = process.env) =>
  tracesSessionExtensions({ baseUrl: env.LANGFUSE_BASE_URL, publicKey: env.LANGFUSE_PUBLIC_KEY, secretKey: env.LANGFUSE_SECRET_KEY, sessionKey: env.QUACK_SESSION_KEY, ...agentOf(env.QUACK_AGENT) });

/** One Langfuse generation per provider response, grouped by the session key as trace and session id. */
export function tracesSessionExtensions(config: TracesConfig): Extension[] {
  if (!config.baseUrl || !config.publicKey || !config.secretKey) return [];
  const sender = new Sender(`${config.baseUrl.replace(/\/+$/, '')}/api/public/ingestion`, `Basic ${Buffer.from(`${config.publicKey}:${config.secretKey}`).toString('base64')}`);
  const traceId = config.sessionKey ?? crypto.randomUUID();
  const pending = new Map<number, { input: unknown; start: number }>();
  return [defineExtension({
    name: 'traces',
    hooks: [hook(GenerationTask, {
      beforeRequest(request, api) {
        remember(pending, api.taskId, { input: request.messages, start: Date.now() });
        return undefined;
      },
      afterResponse(message, api) {
        const { input, start } = pending.get(api.taskId) ?? { input: undefined, start: message.timestamp };
        pending.delete(api.taskId);
        const now = new Date().toISOString();
        const { usage } = message;
        sender.push([
          { id: crypto.randomUUID(), type: 'trace-create', timestamp: now, body: { id: traceId, sessionId: traceId, name: config.agentName } },
          {
            id: crypto.randomUUID(),
            type: 'generation-create',
            timestamp: now,
            body: {
              id: crypto.randomUUID(),
              traceId,
              name: 'generation',
              model: message.model,
              input,
              output: message.content,
              startTime: new Date(start).toISOString(),
              endTime: now,
              level: message.stopReason === 'error' ? 'ERROR' : 'DEFAULT',
              statusMessage: message.errorMessage,
              usageDetails: { input: usage.input, output: usage.output, cache_read: usage.cacheRead, cache_write: usage.cacheWrite, total: usage.totalTokens },
              costDetails: { input: usage.cost.input, output: usage.cost.output, total: usage.cost.total },
              metadata: { agent: config.agentName, version: config.agentVersion, provider: message.provider, stopReason: message.stopReason },
            },
          },
        ]);
      },
    })],
  })];
}
