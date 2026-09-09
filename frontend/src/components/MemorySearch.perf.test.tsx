// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { MemoryTab } from './MemoryTab'
import { client } from '../generated/client.gen'

// React's controlled-input value setter (native setter, bypassing React's
// tracked-value shim) so dispatching 'input' actually registers a change -
// used with fake timers below since userEvent's own internal timers would
// otherwise fight vi.useFakeTimers.
function typeChar(input: HTMLInputElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')!.set!
  setter.call(input, value)
  input.dispatchEvent(new Event('input', { bubbles: true }))
}

client.setConfig({ baseUrl: 'http://localhost' })

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
}

const memWith = (id: string, content: string) => ({
  id, content, bucket: 'repo:x', author: 'a', timestamp: '2026-08-04T18:22:11Z', kind: 'repo',
})

describe('lead C: memory search keystroke behavior', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined
  let listCalls: string[] // q param of each /memories list call, in call order
  let resolvers: Map<number, (v: Response) => void>
  let callIndex: number

  beforeEach(() => {
    // @ts-expect-error react act env flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    listCalls = []
    resolvers = new Map()
    callIndex = 0
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url
      if (url.includes('/memories/stats')) return Promise.resolve(jsonResponse({ weeks: [], scopes: [] }))
      if (url.includes('/memories')) {
        const q = new URL(url).searchParams.get('q') ?? ''
        const idx = callIndex++
        listCalls.push(q)
        // Controllable promise per call - resolves only when the test tells it to.
        return new Promise<Response>(resolve => { resolvers.set(idx, resolve) })
      }
      return Promise.reject(new Error('unexpected fetch ' + url))
    }))
  })

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
    vi.unstubAllGlobals()
  })

  async function renderTab() {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    await act(async () => {
      root!.render(createElement(MemoryTab))
      await new Promise(r => setTimeout(r, 0))
    })
    // Resolve the initial unfiltered-page load (call 0).
    await act(async () => {
      resolvers.get(0)?.(jsonResponse({ memories: [], total: 0 }))
      await new Promise(r => setTimeout(r, 0))
    })
  }

  it('AFTER FIX: debounce collapses keystrokes to one request, sequence guard prevents the race', async () => {
    await renderTab()
    const input = host!.querySelector('input[type="search"]') as HTMLInputElement

    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      const word = 'graphlit'
      for (let i = 1; i <= word.length; i++) {
        await act(async () => {
          typeChar(input, word.slice(0, i))
          await vi.advanceTimersByTimeAsync(50) // well under the 250ms debounce
        })
      }
      // No debounced call yet - only the initial unfiltered load has fired.
      expect(listCalls).toEqual([''])

      await act(async () => { await vi.advanceTimersByTimeAsync(300) }) // let the debounce settle
      console.log('listCalls after typing + debounce settle (fix):', JSON.stringify(listCalls))
      expect(listCalls).toEqual(['', 'graphlit']) // was 9 calls (1 per keystroke) before the fix
    } finally {
      vi.useRealTimers()
    }

    // Resolve the final (correct) call, then simulate a late stale reply
    // arriving afterward for call #0 (the initial empty-query load) - proves
    // the sequence guard, not just the debounce.
    const finalIdx = listCalls.length - 1
    await act(async () => {
      resolvers.get(finalIdx)?.(jsonResponse({ memories: [memWith('correct', 'graphlit result')], total: 1 }))
      await new Promise(r => setTimeout(r, 0))
    })
    expect(host!.textContent).toContain('graphlit result')
    await act(async () => {
      resolvers.get(0)?.(jsonResponse({ memories: [memWith('stale', 'STALE initial page')], total: 1 }))
      await new Promise(r => setTimeout(r, 0))
    })
    const overwritten = host!.textContent!.includes('STALE initial page')
    console.log('stale call #0 overwrote the list after the fix:', overwritten)
    expect(overwritten).toBe(false)
  })

})
