import { AsyncLocalStorage } from 'node:async_hooks';
import type { AssistantMessage, Message } from '@earendil-works/pi-ai';

// One process owns one session, so these are module state. They are how session-half extensions
// see each other: a gate that waits on a person tells a resource gate (capacity) the run is not generating.

/** A session queued for a slot on its model endpoint (a provider id): when it began waiting (epoch ms) and how many are ahead of it.
 * With `error`, the endpoint failed and the run sleeps until its next try: `error` is the last failure. */
export type ModelWait = { since: number; ahead: number; provider: string; error?: string };
let queued: ModelWait | null = null;
let down: ModelWait | null = null;
const waitListeners = new Set<(w: ModelWait | null) => void>();
const publishWait = () => waitListeners.forEach((l) => l(queued ?? down));
/** The session's wait on its model: queued for a slot (the capacity extension sets it), else parked on a failing endpoint (the session sets it). */
export const modelWait = {
  get: () => queued ?? down,
  set(w: ModelWait | null) {
    queued = w;
    publishWait();
  },
  setDown(w: ModelWait | null) {
    down = w;
    publishWait();
  },
  subscribe(listener: (w: ModelWait | null) => void): () => void {
    waitListeners.add(listener);
    return () => void waitListeners.delete(listener);
  },
};

let waiting = 0;
const listeners = new Set<(waiting: number) => void>();
/** How many waits on someone other than the model are open now. */
export const waitingNow = () => waiting;
/** Hears how many waits on someone other than the model (a person, a child session) are open. */
export function onWaiting(listener: (waiting: number) => void): () => void {
  listeners.add(listener);
  return () => void listeners.delete(listener);
}
/** Marks `fn` as a wait on something other than the run's own next model call. */
export async function whileWaiting<T>(fn: () => Promise<T>): Promise<T> {
  waiting += 1;
  listeners.forEach((l) => l(waiting));
  try {
    return await fn();
  } finally {
    waiting -= 1;
    listeners.forEach((l) => l(waiting));
  }
}

const runEndListeners = new Set<() => void>();
/** Hears the session's run end, however it ended: an answer, a failure, an abort or a stop. */
export function onRunEnded(listener: () => void): () => void {
  runEndListeners.add(listener);
  return () => void runEndListeners.delete(listener);
}
/** Called by the session's lifecycle sender when a run ends. */
export const runEnded = () => runEndListeners.forEach((l) => {
  try {
    l();
  } catch {
    // Listeners are best effort: one must not cost the run its run.ended record.
  }
});

/** `provider` names the model endpoint a gate may need to hold capacity on; `signal` lets that wait be cancelled. */
export type GenerationRequest = { systemPrompt?: string; messages: Message[]; provider?: string; signal?: AbortSignal };
export type Generation = GenerationRequest & { start: number; message: AssistantMessage };
const generationListeners = new Set<(g: Generation) => void>();
/** Hears every completion made through `observeGeneration`, for callers that bypass the session's own generation task. */
export function onGeneration(listener: (g: Generation) => void): () => void {
  generationListeners.add(listener);
  return () => void generationListeners.delete(listener);
}
/** Capacity held around each direct completion (a resource gate such as capacity implements it); `after` gets back what `before` returned and runs even when the completion throws.
 * `shared` is true for a completion made inside `inSessionSlot`. */
export type GenerationGate = { before(provider: string, signal?: AbortSignal, shared?: boolean): Promise<unknown>; after(held?: unknown): void };
const sessionSlot = new AsyncLocalStorage<true>();
/** Runs `fn` with its completions under the session's own model capacity: none takes an extra slot or queues behind other sessions, except to take the session's first. */
export const inSessionSlot = <T>(fn: () => Promise<T>): Promise<T> => sessionSlot.run(true, fn);
const generationGates = new Set<GenerationGate>();
export function gateGenerations(gate: GenerationGate): () => void {
  generationGates.add(gate);
  return () => void generationGates.delete(gate);
}

/** Runs one completion and tells `onGeneration` listeners its request, start time (ms) and reply; a listener error never reaches the caller. */
export async function observeGeneration(request: GenerationRequest, complete: (request: GenerationRequest) => Promise<AssistantMessage>): Promise<AssistantMessage> {
  const start = Date.now();
  const sent = [...request.messages];
  const held: Array<[GenerationGate, unknown]> = [];
  let message: AssistantMessage;
  try {
    if (request.provider) {
      for (const g of generationGates) {
        held.push([g, await g.before(request.provider, request.signal, sessionSlot.getStore() ?? false)]);
      }
    }
    message = await complete(request);
  } finally {
    held.forEach(([g, token]) => g.after(token));
  }
  for (const l of generationListeners) {
    try {
      l({ systemPrompt: request.systemPrompt, messages: sent, start, message });
    } catch {
      // Observers are best effort.
    }
  }
  return message;
}
