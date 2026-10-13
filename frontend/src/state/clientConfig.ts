// Caches GET /api/v1/config (trace URL template, build and API versions) and the served SPA's build id. Both
// are fetched on load, on every SSE reconnect and when the tab becomes visible: when a redeploy would show up.
import { useSyncExternalStore } from 'react'
import { api, type ClientConfig } from '../api'
import { API_VERSION } from '../generated/apiVersion.gen'

let config: ClientConfig | undefined
let servedBuild: string | undefined
const listeners = new Set<() => void>()

// The id vite stamped into this page's index.html; absent under the vite dev server.
const ownBuild = typeof document !== 'undefined'
  ? document.querySelector<HTMLMetaElement>('meta[name="quack-build"]')?.content
  : undefined

// 'reload': a different SPA is being served and the page still works. 'older-server': this SPA speaks a newer
// API than the server; reloading would load this same SPA. 'blocked': the server no longer serves our API.
export type Skew = 'ok' | 'reload' | 'older-server' | 'blocked'

// skew compares the SPA's build-time API version and build id with the server's config and the served build id.
// Pure, so it is testable without the fetch.
export function skew(built: number, cfg: ClientConfig | undefined, own: string | undefined, served: string | undefined): Skew {
  if (cfg && cfg.api_min_version > built) return 'blocked'
  // Only a different served SPA warrants a reload: a newer API alone still serves ours, so reloading changes nothing.
  if (own && served && own !== served) return 'reload'
  return cfg && cfg.api_version < built ? 'older-server' : 'ok'
}

// readServedBuild fetches the build id next to the served index.html; undefined when there is none (vite dev
// answers /version.json with index.html).
export async function readServedBuild(): Promise<string | undefined> {
  const res = await fetch('/version.json', { cache: 'no-store' })
  if (!res.ok) return undefined
  const body = await res.json().catch(() => undefined) as { build?: unknown } | undefined
  return typeof body?.build === 'string' ? body.build : undefined
}

export function refreshClientConfig(): Promise<void> {
  return Promise.allSettled([api.getConfig(), readServedBuild()]).then(([c, b]) => {
    if (c.status === 'fulfilled') config = c.value
    if (b.status === 'fulfilled') servedBuild = b.value
    listeners.forEach(l => l())
  })
}

void refreshClientConfig()
// A tab with no open stream still notices a redeploy when the user comes back to it.
if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible') void refreshClientConfig() })
}

function subscribe(l: () => void): () => void {
  listeners.add(l)
  return () => listeners.delete(l)
}

const readSkew = () => skew(API_VERSION, config, ownBuild, servedBuild)

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
