import { useEffect, useState, type DependencyList } from 'react';

export type Loaded<T> = { data: T; error: string };

/** The one way a component reads the API: it reloads when `deps` change, the newest load wins, an answer to an older one is dropped, and a success clears an earlier error. */
export function useLoad<T>(load: () => Promise<T>, deps: DependencyList, initial: T): Loaded<T> {
  const [state, setState] = useState<Loaded<T>>({ data: initial, error: '' });
  useEffect(() => {
    let current = true;
    load().then(
      (data) => current && setState({ data, error: '' }),
      (e: unknown) => current && setState((s) => ({ ...s, error: e instanceof Error ? e.message : String(e) })),
    );
    return () => {
      current = false;
    };
  }, deps);
  return state;
}
