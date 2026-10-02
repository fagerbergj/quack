import type { DiagramSpec } from './diagramSource'
import type { SurfaceContent } from '../lib/a2ui'

// A small webhook-retry PR: four layers, one of each change kind.
export const retrySpec: DiagramSpec = {
  direction: 'TB',
  layers: [
    { id: 'api', title: 'API', description: 'The HTTP surface receivers and operators call. **Unchanged** by this PR.' },
    { id: 'domain', title: 'Domain', description: 'Delivery rules: what is retried, how often, and when a delivery is dead.' },
    { id: 'storage', title: 'Storage', description: 'Postgres tables backing the retry queue.' },
    { id: 'external', title: 'External', description: 'The receiver endpoints we call.' },
  ],
  nodes: [
    { id: 'handler', label: 'POST /events', layer: 'api', change: 'unchanged', detail: 'Accepts an event and calls `deliver`. Not touched.' },
    { id: 'deliver', label: 'deliver()', layer: 'domain', change: 'modified', type: 'behavior', detail: 'Returns a typed `RetryableError` for 5xx and timeouts; a 4xx now goes straight to `dead`.' },
    { id: 'worker', label: 'Retry worker', layer: 'domain', change: 'added', type: 'feature', detail: 'Drains `retry_queue` with `FOR UPDATE SKIP LOCKED`, so two workers never send the same delivery.' },
    { id: 'legacy', label: 'Drop on failure', layer: 'domain', change: 'removed', type: 'behavior', detail: 'The old path that discarded a failed delivery.' },
    { id: 'queue', label: 'retry_queue', layer: 'storage', change: 'added', type: 'feature', detail: 'New table with `attempts` and `next_at` columns.' },
    { id: 'receiver', label: 'Receiver', layer: 'external', change: 'unchanged', detail: 'The customer endpoint. Its status code decides retry or dead.' },
  ],
  edges: [
    { id: 'e_call', from: 'handler', to: 'deliver', label: 'calls', change: 'unchanged', detail: 'Synchronous call; unchanged.' },
    { id: 'e_send', from: 'deliver', to: 'receiver', label: 'POST', change: 'unchanged', detail: 'The HTTP delivery attempt.' },
    { id: 'e_enqueue', from: 'deliver', to: 'queue', label: 'enqueue on 5xx', change: 'added', detail: 'Writes a row with `next_at = now + 2^attempts` seconds.' },
    { id: 'e_drain', from: 'worker', to: 'queue', label: 'claims rows', change: 'added', detail: 'Selects due rows with `SKIP LOCKED`.' },
    { id: 'e_retry', from: 'worker', to: 'deliver', label: 'retries', change: 'added', detail: 'Re-enters `deliver` with the stored attempt count.' },
    { id: 'e_drop', from: 'deliver', to: 'legacy', change: 'removed', detail: 'Failures used to fall through to this branch.' },
  ],
}

// The pr-tutor's Flow and Risk tabs around the diagram, as render_ui stores them.
export const retrySurface: SurfaceContent = {
  surface_id: 'acme-widgets-pr-412-tutor',
  components: [
    { id: 'root', component: 'Card', child: 'tabs' },
    { id: 'tabs', component: 'Tabs', tabs: [{ title: 'Flow', child: 'flow' }, { title: 'Risk', child: 'risk' }] },
    { id: 'flow', component: 'Diagram', ...retrySpec },
    { id: 'risk',
      component: 'RiskTable',
      basis: 'diff-only',
      rows: [
        { change: 'internal/webhook/deliver.go', type: 'behavior', risk: 'high', reason: 'A 4xx now skips retries, so receivers that returned 429 lose deliveries.', blast: 'POST /events handler; every webhook sender in the diff reaches deliver().', tests: 'deliver_test.go covers 5xx and 404, not 429.' },
        { change: 'internal/webhook/retry.go', type: 'feature', risk: 'medium', reason: 'New worker holds row locks while it sends.', blast: 'Only the worker started in main.go.', tests: 'none in the diff' },
        { change: 'migrations/0042_retry_queue.sql', type: 'config', risk: 'low', reason: 'Additive table, no change to existing rows.', blast: 'retry.go only.' },
      ],
    },
  ],
  data_model: {},
}
