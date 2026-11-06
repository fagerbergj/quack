/** Runs at most `n` tasks at once; the rest wait in arrival order. */
export function limiter(n: number) {
  let active = 0;
  const queue: Array<() => void> = [];
  return async <T>(task: () => Promise<T>): Promise<T> => {
    // A finishing task hands its slot straight to the next waiter, so a newcomer cannot slip in between.
    if (active >= n) await new Promise<void>((r) => queue.push(r));
    else active++;
    try {
      return await task();
    } finally {
      const next = queue.shift();
      if (next) next();
      else active--;
    }
  };
}

export const sleepFor = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));
