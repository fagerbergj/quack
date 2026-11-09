import { isRetryableAssistantError, retryDelayMs, type AssistantMessage } from '@earendil-works/pi-ai';

// pi-durable's default (3 retries from 2s) gives up inside 15s; this rides out a model host that is down for about 5 minutes.
export const MODEL_RETRY = { enabled: true, maxRetries: 9, baseDelayMs: 2000, maxAgentDelayMs: 60_000 };

/** Test seams: `sleep` rejects when `signal` aborts; `fetch` is the transport the provider is handed. */
export type RetrySeams = { sleep?: (ms: number, signal?: AbortSignal) => Promise<void>; random?: () => number; fetch?: typeof fetch; log?: (line: string) => void };

const sleepFor = (ms: number, signal?: AbortSignal) => new Promise<void>((resolve, reject) => {
  if (signal?.aborted) return reject(signal.reason);
  const t = setTimeout(resolve, ms);
  signal?.addEventListener('abort', () => (clearTimeout(t), reject(signal.reason)), { once: true });
});

function retryAfterMs(header: string | null): number | undefined {
  if (!header) return undefined;
  const ms = Number.isNaN(Number(header)) ? Date.parse(header) - Date.now() : Number(header) * 1000;
  return Number.isFinite(ms) ? Math.max(0, ms) : undefined;
}

// llama-swap always answers Retry-After: 1, so the provider's ask is a floor under the backoff, not a replacement for it.
function delayMs(attempt: number, header: string | null, random: () => number) {
  const base = Math.max(retryDelayMs(MODEL_RETRY, attempt), retryAfterMs(header) ?? 0);
  return Math.round(Math.min(base, MODEL_RETRY.maxAgentDelayMs) * (1 + 0.25 * random()));
}

const statusOf = (message = '') => /(?<![a-z0-9])[45]\d\d(?![a-z0-9])/i.exec(message)?.[0] ?? message.slice(0, 80);

type Reply = { stopReason: string; errorMessage?: string };

/** Retries a model call's transient failures (429, 5xx, connection, timeout) on MODEL_RETRY; a 4xx or any model answer returns at once. */
// `call` must hand the provider the `fetch` it gets, or Retry-After is never seen; an abort during the backoff rejects.
export async function retryModelCall<T extends Reply>(model: string, call: (send: typeof fetch) => Promise<T>, signal?: AbortSignal, seams: RetrySeams = {}): Promise<T> {
  const { sleep = sleepFor, random = Math.random, fetch: send = globalThis.fetch, log = console.error } = seams;
  let retryAfter: string | null = null;
  const watched: typeof fetch = async (input, init) => {
    const res = await send(input, init);
    retryAfter = res.ok ? null : res.headers.get('retry-after');
    return res;
  };
  for (let attempt = 1; ; attempt++) {
    retryAfter = null;
    const reply = await call(watched);
    if (attempt > MODEL_RETRY.maxRetries || !isRetryableAssistantError(reply as unknown as AssistantMessage)) return reply;
    const ms = delayMs(attempt, retryAfter, random);
    log(`model retry: session=${process.env.QUACK_SESSION_KEY ?? '-'} model=${model} status=${statusOf(reply.errorMessage)} attempt=${attempt}/${MODEL_RETRY.maxRetries} in ${ms}ms`);
    await sleep(ms, signal);
  }
}
