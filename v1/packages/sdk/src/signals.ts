// One process owns one session (A1), so these are module state. They are how session-half extensions
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
