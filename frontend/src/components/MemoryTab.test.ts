// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { MemoryTab } from './MemoryTab'
import { client } from '../generated/client.gen'

// Node's fetch/Request (unlike a browser's) refuses to build a Request from a
// relative URL - it has no document to resolve against. Production serves the
// SPA same-origin so the generated client's relative paths are never an issue
// there; tests need an absolute base for the same requests to construct at all.
client.setConfig({ baseUrl: 'http://localhost' })

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

const MEMORY = {
  id: 'e5f4a1',
  content: "NightsOut's instrumentation tests need minSdk 30 for DEX version 040.",
  bucket: 'repo:NightsOut',
  author: 'code-implementer',
  timestamp: '2026-08-04T18:22:11Z',
  kind: 'repo',
}

describe('MemoryTab', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined
  let fetchMock: ReturnType<typeof vi.fn>

  beforeEach(() => {
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
  })

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
    vi.unstubAllGlobals()
  })

  // The component's own GET fires from a useEffect, so mount + the fetch's
  // promise chain (unwrap → generated SDK → fetch → .text() → JSON.parse →
  // setState) both need flushing before assertions.
  async function renderAndFlush() {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    await act(async () => {
      root!.render(createElement(MemoryTab))
      await new Promise(resolve => setTimeout(resolve, 0))
    })
  }

  function findButton(host: HTMLDivElement, matcher: (b: HTMLButtonElement) => boolean): HTMLButtonElement {
    const found = Array.from(host.querySelectorAll('button')).find(matcher)
    if (!found) throw new Error('button not found')
    return found
  }

  it('renders the list returned by GET /api/v1/memories', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ memories: [MEMORY], total: 1 }))
    await renderAndFlush()

    expect(host!.textContent).toContain(MEMORY.content)
    expect(host!.textContent).toContain('repo:NightsOut')
    expect(fetchMock).toHaveBeenCalledTimes(1)
    const request = fetchMock.mock.calls[0][0] as Request
    expect(request.method).toBe('GET')
    expect(request.url).toContain('/api/v1/memories')
  })

  it('shows an empty state when the list comes back empty', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ memories: [], total: 0 }))
    await renderAndFlush()
    expect(host!.textContent).toContain('No memories yet')
  })

  it('surfaces a fetch failure as an error, not an empty list', async () => {
    fetchMock.mockResolvedValue(new Response('{"error":"index unreachable"}', { status: 500 }))
    await renderAndFlush()
    expect(host!.textContent).not.toContain('No memories yet')
    expect(host!.textContent).toContain('index unreachable')
  })

  it('issues DELETE only after the forget control is confirmed', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ memories: [MEMORY], total: 1 }))
    await renderAndFlush()

    const forgetButton = findButton(host!, b => (b.getAttribute('aria-label') ?? '').startsWith('Forget:'))
    act(() => forgetButton.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true })))

    // Confirm/Cancel is now showing; the GET was the only request so far.
    const confirmButton = findButton(host!, b => b.textContent === 'Confirm')
    expect(fetchMock).toHaveBeenCalledTimes(1)

    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
    await act(async () => {
      confirmButton.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
      await new Promise(resolve => setTimeout(resolve, 0))
    })

    expect(fetchMock).toHaveBeenCalledTimes(2)
    const deleteRequest = fetchMock.mock.calls[1][0] as Request
    expect(deleteRequest.method).toBe('DELETE')
    expect(deleteRequest.url).toContain(`/api/v1/memories/${MEMORY.id}`)
    expect(host!.textContent).not.toContain(MEMORY.content)
  })

  it('cancelling the forget confirmation issues no DELETE', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ memories: [MEMORY], total: 1 }))
    await renderAndFlush()

    const forgetButton = findButton(host!, b => (b.getAttribute('aria-label') ?? '').startsWith('Forget:'))
    act(() => forgetButton.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true })))
    const cancelButton = findButton(host!, b => b.textContent === 'Cancel')
    act(() => cancelButton.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true })))

    expect(fetchMock).toHaveBeenCalledTimes(1) // just the initial GET
    expect(host!.textContent).toContain(MEMORY.content)
  })

  it('omits include_invalidated from the initial GET (default off)', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ memories: [MEMORY], total: 1 }))
    await renderAndFlush()

    const request = fetchMock.mock.calls[0][0] as Request
    expect(request.url).not.toContain('include_invalidated')
  })

  it('toggling "Show invalidated" refetches with include_invalidated=true', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ memories: [MEMORY], total: 1 }))
    await renderAndFlush()
    expect(fetchMock).toHaveBeenCalledTimes(1)

    const toggle = host!.querySelector('input[type="checkbox"]') as HTMLInputElement
    expect(toggle).not.toBeNull()
    expect(toggle.checked).toBe(false)

    fetchMock.mockResolvedValueOnce(jsonResponse({ memories: [MEMORY], total: 1 }))
    await act(async () => {
      // React tracks a checkbox's toggle via the native 'click' event, not
      // 'change' - dispatching MouseEvent click (as the forget-button tests
      // above do) is what actually flips it through React's onChange.
      toggle.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
      await new Promise(resolve => setTimeout(resolve, 0))
    })

    expect(fetchMock).toHaveBeenCalledTimes(2)
    const request = fetchMock.mock.calls[1][0] as Request
    expect(request.url).toContain('include_invalidated=true')
  })
})
