import { useCallback, useEffect, useState, type DependencyList } from 'react';

export type Loaded<T> = { data: T; error: string; reload: () => void };

/** The one way a component reads the API: it reloads when `deps` change or on `reload`, the newest load wins, an answer to an older one is dropped, and a success clears an earlier error. */
export function useLoad<T>(load: () => Promise<T>, deps: DependencyList, initial: T): Loaded<T> {
  const [state, setState] = useState({ data: initial, error: '' });
  const [tick, setTick] = useState(0);
  useEffect(() => {
    let current = true;
    load().then(
      (data) => current && setState({ data, error: '' }),
      (e: unknown) => current && setState((s) => ({ ...s, error: e instanceof Error ? e.message : String(e) })),
    );
    return () => {
      current = false;
    };
  }, [...deps, tick]);
  return { ...state, reload: useCallback(() => setTick((t) => t + 1), []) };
}
