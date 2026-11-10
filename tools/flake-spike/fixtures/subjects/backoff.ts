/** The delay before retry `n`: doubling from `base`, capped at 30 s, plus up to 100 ms of jitter. */
export function backoff(n: number, base: number, random: () => number = Math.random): number {
  return Math.min(base * 2 ** n, 30_000) + Math.floor(random() * 100);
}
