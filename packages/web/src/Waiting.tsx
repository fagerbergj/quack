import type { ModelWait } from './api.ts';

/** A session queued for a model slot: "Waiting…" on screen, the provider named for assistive tech and the tooltip. */
export function Waiting({ w }: { w: ModelWait }) {
  return (
    <span title={`Waiting for ${w.provider} capacity · ${w.ahead} ahead`}>
      <span aria-hidden="true">Waiting…</span>
      <span className="sr-only">Waiting for {w.provider} capacity</span>
    </span>
  );
}
