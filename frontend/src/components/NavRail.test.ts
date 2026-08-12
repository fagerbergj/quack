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

// #746 item 1: the rail's collapsed/expanded state persists across reload
// (localStorage), and Memory is reachable from it as a peer of Chats.

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

  function render(props: Partial<Parameters<typeof NavRail>[0]> = {}) {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    act(() => {
      root!.render(createElement(NavRail, { route: 'chat', ...props }))
    })
  }

  it('shows text labels for both Chats and Memory by default', () => {
    render()
    expect(host!.textContent).toContain('Chats')
    expect(host!.textContent).toContain('Memory')
  })

  // #870: collapsed used to render no rail at all (just a fixed 5px sliver),
  // which lost the whole nav. It now stays a real, clickable icon strip -
  // labels drop but Chats/Memory (and the expand control) stay reachable.
  it('collapses to an icon strip (no text labels, but Chats/Memory/expand stay clickable) when localStorage says so', () => {
    localStorage.setItem('navRailCollapsed', '1')
    render()
    expect(host!.textContent).not.toContain('Chats')
    expect(host!.textContent).not.toContain('Memory')
    expect(host!.querySelector('nav')).not.toBeNull()
    const chatsBtn = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'Chats')
    const memoryBtn = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'Memory')
    const restore = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'Expand navigation')
    expect(chatsBtn).toBeTruthy()
    expect(memoryBtn).toBeTruthy()
    expect(restore).toBeTruthy()
    expect(restore!.tagName).toBe('BUTTON')
  })

  it('persists the collapsed choice to localStorage on toggle, dropping labels but keeping the icon strip clickable', () => {
    render({ initialCollapsed: false })
    const toggle = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'Collapse navigation')!
    act(() => { toggle.dispatchEvent(new MouseEvent('click', { bubbles: true })) })
    expect(localStorage.getItem('navRailCollapsed')).toBe('1')
    expect(host!.textContent).not.toContain('Chats')
    expect(host!.querySelector('nav')).not.toBeNull()
    expect(Array.from(host!.querySelectorAll('button')).some(b => b.getAttribute('aria-label') === 'Chats')).toBe(true)
  })

  it('restore button re-expands the rail (with text labels back) on click', () => {
    localStorage.setItem('navRailCollapsed', '1')
    render()
    const restore = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'Expand navigation')!
    act(() => { restore.dispatchEvent(new MouseEvent('click', { bubbles: true })) })
    expect(localStorage.getItem('navRailCollapsed')).toBe('0')
    expect(host!.textContent).toContain('Chats')
    expect(host!.querySelector('nav')).not.toBeNull()
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
    // The extensions divider is the only border-gray-100 element (the footer's
    // own border-t uses border-gray-200) - absent means no section rendered.
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
