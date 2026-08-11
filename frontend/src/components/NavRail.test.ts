// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { NavRail } from './NavRail'
import { client } from '../generated/client.gen'

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

  it('starts fully collapsed (no rail, no icons, just the restore button) when localStorage says so', () => {
    localStorage.setItem('navRailCollapsed', '1')
    render()
    expect(host!.textContent).not.toContain('Chats')
    expect(host!.textContent).not.toContain('Memory')
    expect(host!.querySelector('nav')).toBeNull()
    const restore = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'Expand navigation')
    expect(restore).toBeTruthy()
    expect(restore!.tagName).toBe('BUTTON')
  })

  it('persists the collapsed choice to localStorage on toggle, removing the rail from the layout', () => {
    render({ initialCollapsed: false })
    const toggle = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'Collapse navigation')!
    act(() => { toggle.dispatchEvent(new MouseEvent('click', { bubbles: true })) })
    expect(localStorage.getItem('navRailCollapsed')).toBe('1')
    expect(host!.textContent).not.toContain('Chats')
    expect(host!.querySelector('nav')).toBeNull()
  })

  it('restore button re-expands the rail on click', () => {
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

  it('renders an extension with a UI descriptor as a real <a href>, not a client-side route', () => {
    render({ initialExtensions: [{ name: 'remarkable', title: 'reMarkable', href: '/remarkable/review' }] })
    const link = host!.querySelector('a[href="/remarkable/review"]')
    expect(link).toBeTruthy()
    expect(link!.textContent).toContain('reMarkable')
  })

  it('renders a UI-less extension name-only, with no link', () => {
    render({ initialExtensions: [{ name: 'noop' }] })
    expect(host!.textContent).toContain('noop')
    expect(host!.querySelector('a')).toBeNull()
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
      expect(host!.querySelector('a[href="/remarkable/review"]')).toBeTruthy()
    } finally {
      vi.unstubAllGlobals()
    }
  })
})
