import { createHash, randomBytes } from 'node:crypto';
import { readFileSync } from 'node:fs';
import type { AssistantMessage } from '@earendil-works/pi-ai';
import { defineExtension, GenerationTask, hook, type Extension } from '@earendil-works/pi-durable';
import { onGeneration } from '@quack/sdk';

export type TracesConfig = { baseUrl?: string; publicKey?: string; secretKey?: string; sessionKey?: string; agentName?: string; agentVersion?: string };

type Span = Record<string, unknown>;

const MAX_QUEUE = 200;
const BATCH = 20;
const SEND_TIMEOUT_MS = 5_000;
const MAX_PENDING = 32;

/** Provider errors do reach afterResponse; the cap is for aborted tasks, whose requests never get one. */
export function remember<V>(map: Map<number, V>, key: number, value: V) {
  if (map.size >= MAX_PENDING) map.delete(map.keys().next().value!);
  map.set(key, value);
}

/** Fire-and-forget OTLP/HTTP JSON batches; a failed batch is dropped and reported once on stderr, the oldest spans go first when the queue is full. */
export class Sender {
  #queue: Span[] = [];
  #running = false;
  #reported = false;
  #url: string;
  #auth: string;
  #report: (text: string) => void;
  constructor(url: string, auth: string, report: (text: string) => void = (t) => void process.stderr.write(t)) {
    this.#url = url;
    this.#auth = auth;
    this.#report = report;
  }

  push(spans: Span[]) {
    this.#queue.push(...spans);
    this.#queue.splice(0, this.#queue.length - MAX_QUEUE);
    if (!this.#running) void this.#drain();
  }

  async #drain() {
    this.#running = true;
    while (this.#queue.length) {
      const spans = this.#queue.splice(0, BATCH);
      const body = { resourceSpans: [{ resource: { attributes: [] }, scopeSpans: [{ scope: { name: 'quack' }, spans }] }] };
      try {
        const res = await fetch(this.#url, { method: 'POST', headers: { authorization: this.#auth, 'content-type': 'application/json', 'x-langfuse-ingestion-version': '4' }, body: JSON.stringify(body), signal: AbortSignal.timeout(SEND_TIMEOUT_MS) });
        const text = await res.text();
        if (!res.ok || rejected(text) > 0) this.#fail(`HTTP ${res.status} ${text.slice(0, 200)}`);
      } catch (err) {
        this.#fail(String(err));
      }
    }
    this.#running = false;
  }

  // Tracing is best effort and must not touch the generation; one stderr line per process is enough to notice it.
  #fail(why: string) {
    if (this.#reported) return;
    this.#reported = true;
    this.#report(`traces: Langfuse did not accept spans, further failures are not reported: ${why}\n`);
  }
}

// OTLP reports partial rejection on a 200 as partialSuccess.rejectedSpans (an int64, so JSON may carry a string).
function rejected(body: string): number {
  try {
    return Number(JSON.parse(body).partialSuccess?.rejectedSpans ?? 0) || 0;
  } catch {
    return 0;
  }
}

// Page images would put base64 megabytes into the span.
const dropImages = function (this: { type?: string }, key: string, value: unknown) {
  return key === 'data' && this?.type === 'image' ? '[image]' : value;
};

const attr = (key: string, value: unknown) => ({ key, value: { stringValue: typeof value === 'string' ? value : JSON.stringify(value) } });
const nanos = (ms: number) => String(BigInt(ms) * 1_000_000n);

/** A generation as an OTLP span with Langfuse's documented attributes; the trace id derives from the session key so a session is one trace. */
export function generationSpan(config: TracesConfig, traceKey: string, input: unknown, start: number, message: AssistantMessage): Span {
  const { usage } = message;
  const failed = message.stopReason === 'error';
  const metadata = { agent: config.agentName, version: config.agentVersion, provider: message.provider, stopReason: message.stopReason };
  const attributes = [
    attr('langfuse.observation.type', 'generation'),
    attr('langfuse.observation.model.name', message.model),
    { key: 'langfuse.observation.input', value: { stringValue: JSON.stringify(input, dropImages) } },
    attr('langfuse.observation.output', message.content),
    attr('langfuse.observation.usage_details', { input: usage.input, output: usage.output, cache_read: usage.cacheRead, cache_write: usage.cacheWrite, total: usage.totalTokens }),
    attr('langfuse.observation.cost_details', { input: usage.cost.input, output: usage.cost.output, cache_read: usage.cost.cacheRead, cache_write: usage.cost.cacheWrite, total: usage.cost.total }),
    attr('langfuse.observation.level', failed ? 'ERROR' : 'DEFAULT'),
    attr('langfuse.session.id', traceKey),
    ...Object.entries(metadata).filter(([, v]) => v !== undefined).map(([k, v]) => attr(`langfuse.observation.metadata.${k}`, v)),
  ];
  if (config.agentName) attributes.push(attr('langfuse.trace.name', config.agentName));
  if (message.errorMessage) attributes.push(attr('langfuse.observation.status_message', message.errorMessage));
  return {
    traceId: createHash('sha256').update(traceKey).digest('hex').slice(0, 32),
    spanId: randomBytes(8).toString('hex'),
    name: 'generation',
    kind: 1,
    startTimeUnixNano: nanos(start),
    endTimeUnixNano: nanos(Math.max(Date.now(), start)),
    attributes,
    status: failed ? { code: 2, message: message.errorMessage } : { code: 1 },
  };
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
  const sender = new Sender(`${config.baseUrl.replace(/\/+$/, '')}/api/public/otel/v1/traces`, `Basic ${Buffer.from(`${config.publicKey}:${config.secretKey}`).toString('base64')}`);
  const traceKey = config.sessionKey ?? crypto.randomUUID();
  const pending = new Map<number, { input: unknown; start: number }>();
  // Workflow model steps call completeSimple directly, so they reach us through the sdk hook instead of GenerationTask.
  onGeneration(({ systemPrompt, messages, start, message }) => {
    const input = systemPrompt ? [{ role: 'system', content: systemPrompt }, ...messages] : messages;
    sender.push([generationSpan(config, traceKey, input, start, message)]);
  });
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
        sender.push([generationSpan(config, traceKey, input, start, message)]);
      },
    })],
  })];
}
