import { useEffect, useState } from 'react'

// AppConfig mirrors the server's /config.json — a filtered, secrets-stripped
// projection of quack.yaml (same shape), served as a plain static file (not an
// openapi/generated-client endpoint). Only the bits the frontend needs are
// modeled here; extensions.github is present iff the GitHub extension is
// configured server-side.
export interface AppConfig {
  extensions?: {
    github?: Record<string, unknown>
  }
}

let configPromise: Promise<AppConfig | null> | null = null

// fetchAppConfig is memoized process-wide: /config.json doesn't change during
// a page session, and every consumer (useAppConfig) shares the one request.
// A failed or missing fetch resolves to null — callers must default
// optimistically (config ? ... : true) so a config outage never hides UI.
function fetchAppConfig(): Promise<AppConfig | null> {
  if (!configPromise) {
    configPromise = fetch('/config.json')
      .then(res => (res.ok ? (res.json() as Promise<AppConfig>) : null))
      .catch(() => null)
  }
  return configPromise
}

// useAppConfig loads /config.json once and returns it; undefined until it
// resolves (still loading), null on a failed/missing fetch.
export function useAppConfig(): AppConfig | null | undefined {
  const [config, setConfig] = useState<AppConfig | null | undefined>(undefined)
  useEffect(() => {
    let cancelled = false
    fetchAppConfig().then(c => {
      if (!cancelled) setConfig(c)
    })
    return () => {
      cancelled = true
    }
  }, [])
  return config
}
