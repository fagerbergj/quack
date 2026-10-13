import { renderToStaticMarkup } from 'react-dom/server'
import { afterEach, describe, it, expect, vi } from 'vitest'
import { VersionBanner } from './VersionBanner'
import { readServedBuild, skew } from '../state/clientConfig'
import type { ClientConfig } from '../api'

const cfg = (over: Partial<ClientConfig> = {}): ClientConfig => ({ api_version: 2, api_min_version: 2, version: '1.0.0', ...over })

describe('skew', () => {
  it.each([
    ['nothing loaded yet', undefined, 'aaa', undefined, 'ok'],
    ['same API and SPA build', cfg(), 'aaa', 'aaa', 'ok'],
    ['server rebuilt but serves the same SPA', cfg({ version: '1.0.1' }), 'aaa', 'aaa', 'ok'],
    ['a different SPA is served', cfg(), 'aaa', 'bbb', 'reload'],
    ['a different SPA is served before config loads', undefined, 'aaa', 'bbb', 'reload'],
    ['no build id under vite dev', cfg(), undefined, undefined, 'ok'],
    ['server API is newer but still serves ours, same SPA', cfg({ api_version: 3 }), 'aaa', 'aaa', 'ok'],
    ['server API is older (rolled back), same SPA', cfg({ api_version: 1, api_min_version: 1 }), 'aaa', 'aaa', 'older-server'],
    ['server API is older and its own SPA is served', cfg({ api_version: 1, api_min_version: 1 }), 'aaa', 'old', 'reload'],
    ['server dropped our API version', cfg({ api_version: 3, api_min_version: 3 }), 'aaa', 'bbb', 'blocked'],
  ] as const)('%s', (_name, c, own, served, want) => {
    expect(skew(2, c, own, served)).toBe(want)
  })
})

describe('readServedBuild', () => {
  afterEach(() => { vi.unstubAllGlobals() })
  const serve = (res: Response) => vi.stubGlobal('fetch', vi.fn(async () => res))

  it('reads the build id, bypassing the HTTP cache', async () => {
    serve(new Response('{"build":"abc123"}'))
    expect(await readServedBuild()).toBe('abc123')
    expect(fetch).toHaveBeenCalledWith('/version.json', { cache: 'no-store' })
  })

  it.each([
    ['a 404', new Response('not found', { status: 404 })],
    ['the vite dev index.html fallback', new Response('<!doctype html>')],
    ['JSON without a build id', new Response('{"version":1}')],
  ])('is undefined for %s', async (_name, res) => {
    serve(res)
    expect(await readServedBuild()).toBeUndefined()
  })
})

describe('VersionBanner', () => {
  it('renders nothing when versions match', () => {
    expect(renderToStaticMarkup(<VersionBanner skew="ok" />)).toBe('')
  })

  it('offers a non-blocking reload strip', () => {
    const html = renderToStaticMarkup(<VersionBanner skew="reload" />)
    expect(html).toContain('role="status"')
    expect(html).toContain('quack was updated')
    expect(html).toContain('Reload')
    expect(html).not.toContain('fixed inset-0')
  })

  it('explains an older server without offering a reload that would load this same page', () => {
    const html = renderToStaticMarkup(<VersionBanner skew="older-server" />)
    expect(html).toContain('role="status"')
    expect(html).toContain('newer than the quack server')
    expect(html).not.toContain('Reload')
    expect(html).not.toContain('fixed inset-0')
  })

  it('blocks the page when the server no longer serves this build', () => {
    const html = renderToStaticMarkup(<VersionBanner skew="blocked" />)
    expect(html).toContain('role="alertdialog"')
    expect(html).toContain('fixed inset-0')
    expect(html).toContain('Reload')
  })
})
