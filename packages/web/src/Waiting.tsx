import type { ModelWait } from './api.ts';

/** A session queued for a model slot or waiting to retry its endpoint: "Waiting…" on screen, the provider and why for assistive tech and the tooltip. */
export function Waiting({ w }: { w: ModelWait }) {
  const why = w.error ? `Waiting to retry ${w.provider} · ${w.error}` : `Waiting for ${w.provider} capacity · ${w.ahead} ahead`;
  return (
    <span title={why}>
      <span aria-hidden="true">Waiting…</span>
      <span className="sr-only">{why}</span>
    </span>
  );
}
