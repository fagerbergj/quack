/** Runs every task at once and returns the results in the order they finished. */
export async function settleAll<T>(tasks: Array<() => Promise<T>>): Promise<T[]> {
  const done: T[] = [];
  await Promise.all(tasks.map(async (t) => done.push(await t())));
  return done;
}
