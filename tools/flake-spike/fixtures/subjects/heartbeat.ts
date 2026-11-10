/** Calls beat every `ms` until the returned stop is called. */
export function startHeartbeat(beat: () => void, ms: number): () => void {
  const t = setInterval(beat, ms);
  return () => clearInterval(t);
}
