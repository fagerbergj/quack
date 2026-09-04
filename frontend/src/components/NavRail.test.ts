// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { NavRail } from './NavRail'
import { client } from '../generated/client.gen'
import type { ExtensionInfo } from '../api'

// Node's fetch/Request (unlike a browser's) refuses to build a Request from a
// relative URL - see MemoryTab.test.ts for the same setup.
client.setConfig({ baseUrl: 'http://localhost' })

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

// #1171: NavRail is a pure drawer - the open state is owned by the caller
// (App.tsx) and never persisted, so these tests drive it via the open prop
// and assert the key the rail used to persist (navRailCollapsed) is neither
// read nor written.

describe('NavRail', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  beforeEach(() => {
    localStorage.clear()
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
  })

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
    window.history.replaceState(null, '', '/') // undo any /ext/:name navigate() from the click test above
  })

  // open defaults to true so the content tests exercise the drawer body;
  // onClose is a no-op in these unit tests (closing is the App's job).
  function render(props: Partial<Parameters<typeof NavRail>[0]> = {}) {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    act(() => {
      root!.render(createElement(NavRail, { route: 'chat', open: true, onClose: () => {}, ...props }))
    })
  }

  it('shows text labels for both Chats and Memory by default', () => {
    render()
    expect(host!.textContent).toContain('Chats')
    expect(host!.textContent).toContain('Memory')
  })

  // #1171: the drawer remembers nothing. A stale navRailCollapsed value from
  // before the rail was deleted is neither read (the drawer stays closed
  // regardless) nor rewritten by rendering.
  it('ignores a stale navRailCollapsed localStorage value - neither read nor written', () => {
    localStorage.setItem('navRailCollapsed', '1')
    render({ open: false })
    expect(host!.textContent).toBe('') // closed renders zero DOM
    expect(host!.querySelector('nav')).toBeNull()
    expect(document.querySelector('[role="dialog"]')).toBeNull()
    expect(localStorage.getItem('navRailCollapsed')).toBe('1') // untouched, never rewritten
  })

  it('marks the active route via aria-current', () => {
    render({ route: 'memory' })
    const memoryBtn = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'Memory')!
    const chatsBtn = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'Chats')!
    expect(memoryBtn.getAttribute('aria-current')).toBe('page')
    expect(chatsBtn.getAttribute('aria-current')).toBeNull()
  })

  // #870: extension entries navigate client-side to this app's own
  // /ext/:name host page (ExtensionHost), not a real <a href> that would
  // leave the SPA (and the rail) behind.
  it('renders an extension with a UI descriptor as a client-side /ext/:name nav button', () => {
    render({ initialExtensions: [{ name: 'remarkable', title: 'reMarkable', href: '/remarkable/review' }] })
    expect(host!.querySelector('a')).toBeNull()
    const btn = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'reMarkable')
    expect(btn).toBeTruthy()
    expect(btn!.textContent).toContain('reMarkable')
    act(() => { btn!.dispatchEvent(new MouseEvent('click', { bubbles: true })) })
    expect(window.location.pathname).toBe('/ext/remarkable')
  })

  // #870 (Jason): a UI-less extension has nowhere to navigate to - it must
  // not render at all, not even inert.
  it('renders nothing for a UI-less extension (no href)', () => {
    render({ initialExtensions: [{ name: 'noop' }] })
    expect(host!.textContent).not.toContain('noop')
    expect(host!.querySelector('a')).toBeNull()
    expect(Array.from(host!.querySelectorAll('button')).some(b => b.getAttribute('aria-label') === 'noop')).toBe(false)
  })

  it('renders no extensions section when the only extension has no href', () => {
    render({ initialExtensions: [{ name: 'github' }] })
    // The extensions divider is the only border-gray-100 element in the drawer
    // panel - absent means no section rendered.
    expect(host!.querySelector('.border-gray-100')).toBeNull()
  })

  it('renders only the href-bearing extension out of a mixed list', () => {
    render({ initialExtensions: [{ name: 'github' }, { name: 'usage', title: 'Usage', href: '/usage' }] })
    expect(host!.textContent).not.toContain('github')
    const usageBtn = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'Usage')
    expect(usageBtn).toBeTruthy()
  })

  // Defensive read of a field not yet in the generated ExtensionInfo type
  // (wire schema addition landing separately) - falls back to 🧩 when absent.
  it('shows the extension-provided icon when present, and 🧩 as the fallback', () => {
    render({
      initialExtensions: [
        { name: 'usage', title: 'Usage', href: '/usage', icon: '📊' } as ExtensionInfo,
        { name: 'remarkable', title: 'reMarkable', href: '/remarkable/review' },
      ],
    })
    const usageBtn = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'Usage')!
    const remarkableBtn = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'reMarkable')!
    expect(usageBtn.textContent).toContain('📊')
    expect(remarkableBtn.textContent).toContain('🧩')
  })

  it('renders no extensions section when the list is empty', () => {
    render({ initialExtensions: [] })
    expect(host!.querySelector('a')).toBeNull()
  })

  it('fetches GET /api/v1/extensions itself when no seam prop is given', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse([{ name: 'remarkable', title: 'reMarkable', href: '/remarkable/review' }]))
    vi.stubGlobal('fetch', fetchMock)
    try {
      await act(async () => {
        render()
        await new Promise(resolve => setTimeout(resolve, 0))
      })
      expect(fetchMock).toHaveBeenCalledTimes(1)
      const request = fetchMock.mock.calls[0][0] as Request
      expect(request.url).toContain('/api/v1/extensions')
      expect(Array.from(host!.querySelectorAll('button')).some(b => b.getAttribute('aria-label') === 'reMarkable')).toBe(true)
    } finally {
      vi.unstubAllGlobals()
    }
  })
})
