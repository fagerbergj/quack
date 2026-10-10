// Caches GET /api/v1/config for the process lifetime - deployment-level
// values (currently just the trace URL template) that never change mid-session.
import { api } from '../api'

let template: string | undefined
let version: string | undefined
let fetched = false

void api.getConfig().then(c => { template = c.otel_trace_url_template; version = c.version }).catch(() => {}).finally(() => { fetched = true })

// Renders traceId via the server's otel_trace_url_template; undefined if either is unset or config hasn't loaded
// yet (a later re-render picks it up).
export function traceUrl(traceId: string | undefined): string | undefined {
  if (!fetched || !template || !traceId) return undefined
  return template.replace('{trace_id}', traceId)
}

// serverVersion returns the server's build version once the config fetch
// resolves, else undefined (NavRail renders nothing for that render pass).
export function serverVersion(): string | undefined {
  return version
}
