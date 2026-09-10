// @vitest-environment jsdom
// A lazy route that fails once (e.g. a bad chunk fetch) must not keep failing:
// routes share one JSX slot, so React reuses the LazyLoadBoundary and its `failed` state survives navigation, white-screening a never-broken route.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import App from './App'
import { ChatStoreProvider } from './state/ChatStoreProvider'
import { client } from './generated/client.gen'
import { navigate } from './router'

client.setConfig({ baseUrl: 'http://localhost' })

vi.mock('./pages/Memory', () => ({
  default: () => { throw new Error('Memory chunk failed to render') },
}))

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

function mockMatchMedia() {
  vi.stubGlobal('matchMedia', vi.fn().mockImplementation((query: string) => ({
    matches: false,
    media: query,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  })))
}

function stubFetch() {
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const url = input instanceof URL ? input.href : typeof input === 'string' ? input : (input as Request).url
    if (url.includes('/api/v1/chats')) return jsonResponse({ data: [] })
    if (url.includes('/api/v1/extensions')) return jsonResponse([{ name: 'usage', title: 'Usage', href: '/usage' }])
    if (url.includes('/api/v1/config')) return jsonResponse({})
    return jsonResponse({}, 404)
  }))
}

describe('App - a failed lazy route must not stay failed after navigating away (#1299)', () => {
  beforeEach(() => {
    mockMatchMedia()
    stubFetch()
    vi.spyOn(console, 'error').mockImplementation(() => {})
    window.history.replaceState(null, '', '/memory')
  })

  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  it('a broken Memory route recovers once the user navigates to ExtensionHost', async () => {
    render(<ChatStoreProvider><App /></ChatStoreProvider>)
    expect(await screen.findByRole('button', { name: 'Reload' })).toBeTruthy()

    navigate('/ext/usage')

    // The stale error screen from the unrelated Memory failure must be gone -
    // ExtensionHost gets its own fresh mount, not the same failed instance.
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Reload' })).toBeNull())
    await waitFor(() => expect(document.querySelector('iframe')).toBeTruthy())
  })
})
