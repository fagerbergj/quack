// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { MemoryTab } from './MemoryTab'
import { api } from '../api'
import { client } from '../generated/client.gen'

// Node's fetch/Request (unlike a browser's) refuses to build a Request from a
// relative URL - no document to resolve against. Production serves the SPA
// same-origin so relative paths are fine there; tests need an absolute base for the same requests to construct at all.
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
  let fetchMock: ReturnType<typeof vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>>

  beforeEach(() => {
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    fetchMock = vi.fn()
    // MemoryTab now also fires an independent GET /memories/stats (#1267) on
    // mount; intercept it ahead of fetchMock so every existing test's call
    // count/indexing still refers only to the memories-list/vote/delete requests.
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url
      if (url.includes('/memories/stats')) {
        return Promise.resolve(jsonResponse({ weeks: [], scopes: [] }))
      }
      return fetchMock(input, init)
    }))
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

    const kebabButton = findButton(host!, b => (b.getAttribute('aria-label') ?? '') === 'Memory actions')
    act(() => kebabButton.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true })))
    const forgetButton = findButton(host!, b => b.textContent?.trim() === 'Forget')
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

    const kebabButton = findButton(host!, b => (b.getAttribute('aria-label') ?? '') === 'Memory actions')
    act(() => kebabButton.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true })))
    const forgetButton = findButton(host!, b => b.textContent?.trim() === 'Forget')
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

  // epic #1255 P4: clicking an arrow sends the vote request and updates the
  // score optimistically; a second click on the now-active arrow toggles it off.
  it('vote click sends the request and updates optimistically; toggle removes it', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ memories: [{ ...MEMORY, vote_score: 0, upvotes: 0 }], total: 1 }))
    await renderAndFlush()

    const upButton = findButton(host!, b => (b.getAttribute('aria-label') ?? '') === 'Upvote')

    fetchMock.mockResolvedValueOnce(jsonResponse({ ...MEMORY, vote_score: 1, upvotes: 1, tier: 'verified', own_vote: 'up' }))
    await act(async () => {
      upButton.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
      await new Promise(resolve => setTimeout(resolve, 0))
    })

    expect(fetchMock).toHaveBeenCalledTimes(2)
    const voteRequest = fetchMock.mock.calls[1][0] as Request
    expect(voteRequest.method).toBe('POST')
    expect(voteRequest.url).toContain(`/api/v1/memories/${MEMORY.id}/vote`)
    expect(host!.textContent).toContain('verified')

    // Optimistic update happens before the response lands - re-fetch the
    // (now re-rendered) button reference and click it again to toggle off.
    fetchMock.mockResolvedValueOnce(jsonResponse({ ...MEMORY, vote_score: 0, upvotes: 0, tier: 'verified' }))
    const upButtonAgain = findButton(host!, b => (b.getAttribute('aria-label') ?? '') === 'Upvote')
    await act(async () => {
      upButtonAgain.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
      await new Promise(resolve => setTimeout(resolve, 0))
    })

    expect(fetchMock).toHaveBeenCalledTimes(3)
    const toggleRequest = fetchMock.mock.calls[2][0] as Request
    const body = await toggleRequest.clone().json()
    expect(body.vote).toBe('none')
  })

  // #1300 review finding 1: a synchronous throw from voteMemory reaches
  // handleVote's catch before React has flushed the setMemories updater, so
  // the pre-vote row must come from a ref, not a variable set inside that updater.
  it('rolls back the optimistic vote when voteMemory rejects synchronously', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ memories: [{ ...MEMORY, vote_score: 0, upvotes: 0 }], total: 1 }))
    await renderAndFlush()

    const upButton = findButton(host!, b => (b.getAttribute('aria-label') ?? '') === 'Upvote')
    const voteSpy = vi.spyOn(api, 'voteMemory').mockImplementation(() => {
      throw new Error('boom')
    })

    await act(async () => {
      upButton.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
      await new Promise(resolve => setTimeout(resolve, 0))
    })

    expect(host!.textContent).toContain('Vote failed')
    const upButtonAfter = findButton(host!, b => (b.getAttribute('aria-label') ?? '') === 'Upvote')
    expect(upButtonAfter.getAttribute('aria-pressed')).toBe('false')
    voteSpy.mockRestore()
  })
})
