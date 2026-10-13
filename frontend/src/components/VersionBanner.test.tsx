import { renderToStaticMarkup } from 'react-dom/server'
import { describe, it, expect } from 'vitest'
import { VersionBanner } from './VersionBanner'
import { skew } from '../state/clientConfig'
import type { ClientConfig } from '../api'

const cfg = (over: Partial<ClientConfig> = {}): ClientConfig => ({ api_version: 2, api_min_version: 2, version: '1.0.0', ...over })

describe('skew', () => {
  it.each([
    ['no config yet', undefined, undefined, 'ok'],
    ['same API and build', cfg(), '1.0.0', 'ok'],
    ['build changed under the tab', cfg({ version: '1.0.1' }), '1.0.0', 'reload'],
    ['server API is newer but still serves ours', cfg({ api_version: 3 }), '1.0.0', 'reload'],
    ['server API is older (rolled back)', cfg({ api_version: 1, api_min_version: 1 }), '1.0.0', 'reload'],
    ['server dropped our API version', cfg({ api_version: 3, api_min_version: 3 }), '1.0.0', 'blocked'],
  ] as const)('%s', (_name, c, first, want) => {
    expect(skew(2, c, first)).toBe(want)
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

  it('blocks the page when the server no longer serves this build', () => {
    const html = renderToStaticMarkup(<VersionBanner skew="blocked" />)
    expect(html).toContain('role="alertdialog"')
    expect(html).toContain('fixed inset-0')
    expect(html).toContain('Reload')
  })
})
