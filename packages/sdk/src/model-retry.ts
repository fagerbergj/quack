import { createAssistantMessageEventStream, isRetryableAssistantError, retryDelayMs, type AssistantMessage, type AssistantMessageEventStream, type Provider } from '@earendil-works/pi-ai';

// In-process retries (the judge, decide) and sessions with no deployment behind them ride out about 5 minutes; a deployed session's runs wait for hours.
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

// An input over the model's context fails the same way every time, whatever status the host gives it (llama.cpp: 500).
export const inputTooLarge = (reply: Reply) => reply.stopReason === 'error' && /too large|exceeds? the (?:available )?context|context (?:length|size|window)/i.test(reply.errorMessage ?? '');

/** A failure retrying can clear (429, 5xx, connection, timeout, an engine restart); a 4xx refusal, an input over the context or a model answer cannot. */
export const transientModelError = (reply: Reply) => !inputTooLarge(reply) && isRetryableAssistantError(reply as unknown as AssistantMessage);

// Strata refuses these while its pool is full or its engine restarts (about 2 min); both clear on their own, unlike a prompt over the pool.
// Both halves are the engine's words: the pool refusal is its log line for a full lane (#2487), the other what server.py returns to a request waiting through a restart.
const ENGINE_BUSY = /K\/V pool is full \(\d+ positions on this lane, \d+ more pages wanted, \d+ free\)|the engine stopped while this request waited; it was not sent/;

/** Wraps a provider so an engine-busy reply reads as `server_busy`: pi-ai's retry (pi-durable runs and `retryModelCall` alike) matches text only. */
export function retryEngineBusy<P extends Provider>(provider: P): P {
  const busy = (m: AssistantMessage) => (m.stopReason === 'error' && ENGINE_BUSY.test(m.errorMessage ?? '') ? { ...m, errorMessage: `server_busy ${m.errorMessage}` } : m);
  const wrap = (stream: unknown) => (...args: unknown[]) => {
    const source = (stream as (...a: unknown[]) => AssistantMessageEventStream).apply(provider, args);
    const out = createAssistantMessageEventStream();
    void (async () => {
      try {
        for await (const e of source) {
          const event = e.type === 'error' ? { ...e, error: busy(e.error) } : e;
          out.push(event);
        }
      } finally {
        // The wrapped provider must end its stream with a done or error event; lazyApi guarantees that, a hand-written provider must too.
        out.end();
      }
    })();
    return out;
  };
  return new Proxy(provider, { get: (t, k) => (k === 'stream' || k === 'streamSimple' ? wrap(t[k]) : Reflect.get(t, k, t)) });
}

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
    if (attempt > MODEL_RETRY.maxRetries || !transientModelError(reply)) return reply;
    const ms = delayMs(attempt, retryAfter, random);
    log(`model retry: session=${process.env.QUACK_SESSION_KEY ?? '-'} model=${model} status=${statusOf(reply.errorMessage)} attempt=${attempt}/${MODEL_RETRY.maxRetries} in ${ms}ms`);
    await sleep(ms, signal);
  }
}
