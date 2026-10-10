// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { MemoryTab } from './MemoryTab'
import { client } from '../generated/client.gen'

// The native value setter bypasses React's tracked-value shim so 'input' registers a change; used instead
// of userEvent, whose internal timers fight vi.useFakeTimers.
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

describe('memory search', () => {
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

  it('debounces keystrokes into one request and ignores a stale earlier reply', async () => {
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
      expect(listCalls).toEqual(['', 'graphlit'])
    } finally {
      vi.useRealTimers()
    }

    // Resolve the final call, then let a stale reply for the initial empty-query load arrive late:
    // this proves the sequence guard, not just the debounce.
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
    expect(host!.textContent).not.toContain('STALE initial page')
  })
})
