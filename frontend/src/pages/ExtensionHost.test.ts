// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { createElement } from 'react'
import { act, cleanup, render } from '@testing-library/react'
import ExtensionHost from './ExtensionHost'
import { client } from '../generated/client.gen'

afterEach(cleanup)

// Node's fetch/Request (unlike a browser's) refuses to build a Request from a
// relative URL - see MemoryTab.test.ts for the same setup.
client.setConfig({ baseUrl: 'http://localhost' })

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}


describe('ExtensionHost', () => {
  let host: HTMLElement | undefined

  function mount(props: Partial<Parameters<typeof ExtensionHost>[0]> = {}) {
    host = render(createElement(ExtensionHost, props)).container
  }

  it('renders an iframe with src and title from the matching extension', () => {
    mount({
      name: 'remarkable',
      initialExtensions: [{ name: 'remarkable', title: 'reMarkable', href: '/remarkable/review' }],
    })
    const iframe = host!.querySelector('iframe')
    expect(iframe).toBeTruthy()
    expect(iframe!.getAttribute('src')).toBe('/remarkable/review')
    expect(iframe!.getAttribute('title')).toBe('reMarkable')
  })

  it('falls back to the extension name as the title when no title is set', () => {
    mount({
      name: 'usage',
      initialExtensions: [{ name: 'usage', href: '/usage' }],
    })
    const iframe = host!.querySelector('iframe')
    expect(iframe!.getAttribute('title')).toBe('usage')
  })

  it('sandboxes the iframe to same-origin/scripts/forms/user-activated top nav only', () => {
    mount({
      name: 'usage',
      initialExtensions: [{ name: 'usage', href: '/usage' }],
    })
    const iframe = host!.querySelector('iframe')!
    expect(iframe.getAttribute('sandbox')).toBe(
      'allow-same-origin allow-scripts allow-forms allow-top-navigation-by-user-activation',
    )
  })

  it('shows a not-found message for a name with no matching href-bearing extension', () => {
    mount({ name: 'missing', initialExtensions: [{ name: 'usage', href: '/usage' }] })
    expect(host!.querySelector('iframe')).toBeNull()
    expect(host!.textContent).toContain('not found')
  })

  it('shows a not-found message for a UI-less extension (no href)', () => {
    mount({ name: 'github', initialExtensions: [{ name: 'github' }] })
    expect(host!.querySelector('iframe')).toBeNull()
    expect(host!.textContent).toContain('not found')
  })

  it('fetches GET /api/v1/extensions itself when no seam prop is given', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse([{ name: 'usage', title: 'Usage', href: '/usage' }]))
    vi.stubGlobal('fetch', fetchMock)
    try {
      await act(async () => {
        mount({ name: 'usage' })
        await new Promise(resolve => setTimeout(resolve, 0))
      })
      expect(fetchMock).toHaveBeenCalledTimes(1)
      const request = fetchMock.mock.calls[0][0] as Request
      expect(request.url).toContain('/api/v1/extensions')
      expect(host!.querySelector('iframe')?.getAttribute('src')).toBe('/usage')
    } finally {
      vi.unstubAllGlobals()
    }
  })
})
