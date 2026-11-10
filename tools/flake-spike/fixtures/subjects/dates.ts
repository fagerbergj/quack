/** The local calendar day an event falls on, as YYYY-MM-DD. */
export function day(at: Date = new Date()): string {
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}`;
}

/** A record stamped with the moment it was made. */
export function stamp<T extends object>(value: T, now: () => number = Date.now): T & { at: number } {
  return { ...value, at: now() };
}
