// Caches GET /api/v1/config - deployment-level values (trace URL template, build and API versions). Fetched on
// load and again on every SSE reconnect, the moment a redeployed server would show up.
import { useSyncExternalStore } from 'react'
import { api, type ClientConfig } from '../api'
import { API_VERSION } from '../generated/apiVersion.gen'

let config: ClientConfig | undefined
let loadedVersion: string | undefined
const listeners = new Set<() => void>()

// 'reload': a newer (or different) quack is serving, the page still works. 'blocked': the server no longer
// serves the API this page was built against.
export type Skew = 'ok' | 'reload' | 'blocked'

// skew compares the SPA's build-time API version and the build version seen at page load with the server's
// config. Pure, so it is testable without the fetch.
export function skew(built: number, cfg: ClientConfig | undefined, firstVersion: string | undefined): Skew {
  if (!cfg) return 'ok'
  if (cfg.api_min_version > built) return 'blocked'
  if (cfg.api_version !== built) return 'reload'
  // The SPA ships inside the server binary, so a changed build version means a newer SPA is being served.
  return firstVersion !== undefined && cfg.version !== firstVersion ? 'reload' : 'ok'
}

export function refreshClientConfig(): Promise<void> {
  return api.getConfig().then(c => {
    config = c
    loadedVersion ??= c.version
    listeners.forEach(l => l())
  }).catch(() => {})
}

void refreshClientConfig()

function subscribe(l: () => void): () => void {
  listeners.add(l)
  return () => listeners.delete(l)
}

const readSkew = () => skew(API_VERSION, config, loadedVersion)

export function useSkew(): Skew {
  return useSyncExternalStore(subscribe, readSkew, () => 'ok')
}

// Renders traceId via the server's otel_trace_url_template; undefined if either is unset or config hasn't loaded
// yet (a later re-render picks it up).
export function traceUrl(traceId: string | undefined): string | undefined {
  const template = config?.otel_trace_url_template
  if (!template || !traceId) return undefined
  return template.replace('{trace_id}', traceId)
}

// serverVersion returns the server's build version once the config fetch
// resolves, else undefined (NavRail renders nothing for that render pass).
export function serverVersion(): string | undefined {
  return config?.version
}
