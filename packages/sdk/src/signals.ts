import type { AssistantMessage, Message } from '@earendil-works/pi-ai';

// One process owns one session, so these are module state. They are how session-half extensions
// see each other: a gate that waits on a person tells a resource gate (slots) the run is not generating.
let note: string | null = null;
/** A short status the session reports through /ping (for example "Waiting for a model slot (2 ahead)"); null clears it. */
export const statusNote = { get: () => note, set: (text: string | null) => void (note = text) };

let waiting = 0;
const listeners = new Set<(waiting: number) => void>();
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

/** `provider` names the model endpoint a gate may need to hold capacity on; `signal` lets that wait be cancelled. */
export type GenerationRequest = { systemPrompt?: string; messages: Message[]; provider?: string; signal?: AbortSignal };
export type Generation = GenerationRequest & { start: number; message: AssistantMessage };
const generationListeners = new Set<(g: Generation) => void>();
/** Hears every completion made through `observeGeneration`, for callers that bypass the session's own generation task. */
export function onGeneration(listener: (g: Generation) => void): () => void {
  generationListeners.add(listener);
  return () => void generationListeners.delete(listener);
}
/** Capacity held around each direct completion (a resource gate such as slots implements it); `after` gets back what `before` returned and runs even when the completion throws. */
export type GenerationGate = { before(provider: string, signal?: AbortSignal): Promise<unknown>; after(held?: unknown): void };
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
        held.push([g, await g.before(request.provider, request.signal)]);
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
