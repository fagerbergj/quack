// Caches GET /api/v1/config for the process lifetime - deployment-level
// values (currently just the trace URL template) that never change mid-session.
import { api } from '../api'

let template: string | undefined
let fetched = false

void api.getConfig().then(c => { template = c.otel_trace_url_template }).catch(() => {}).finally(() => { fetched = true })

// traceUrl renders traceId through the server's otel_trace_url_template, or
// undefined if either is unset (no link should be rendered) or the config
// fetch hasn't resolved yet (a later re-render, e.g. on the next SSE event,
// picks it up - see DagNode's live event volume).
export function traceUrl(traceId: string | undefined): string | undefined {
  if (!fetched || !template || !traceId) return undefined
  return template.replace('{trace_id}', traceId)
}
