export type Failure<T> = { item: T; error: Error };

/** Runs `work` over items with `concurrency` workers; one item's failure is collected, never lost with its siblings' results. */
export async function eachItem<T, R>(items: T[], concurrency: number, work: (item: T) => Promise<R>): Promise<{ results: R[]; failures: Failure<T>[] }> {
  const queue = [...items];
  const results: R[] = [];
  const failures: Failure<T>[] = [];
  await Promise.all(Array.from({ length: concurrency }, async () => {
    for (let item = queue.shift(); item !== undefined; item = queue.shift()) {
      try {
        results.push(await work(item));
      } catch (err) {
        failures.push({ item, error: err instanceof Error ? err : new Error(String(err)) });
      }
    }
  }));
  return { results, failures };
}
