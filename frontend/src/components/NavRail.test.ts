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
  // (wire schema addition landing separately). A Material icon name renders
  // as that icon; an inline `<svg>` renders as-is; anything else (including
  // a raw emoji - the legacy shape) falls back to the generic "extension"
  // glyph rather than rendering arbitrary plugin-supplied emoji.
  it('accepts a Material icon name or inline SVG, and falls back to the generic glyph', () => {
    render({
      initialExtensions: [
        { name: 'usage', title: 'Usage', href: '/usage', icon: 'memory' } as ExtensionInfo,
        { name: 'custom', title: 'Custom', href: '/custom', icon: '<svg viewBox="0 0 24 24"><path d="M1 1h1v1h-1z"/></svg>' } as ExtensionInfo,
        { name: 'remarkable', title: 'reMarkable', href: '/remarkable/review', icon: '📊' } as ExtensionInfo,
      ],
    })
    const usageBtn = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'Usage')!
    const customBtn = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'Custom')!
    const remarkableBtn = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'reMarkable')!
    expect(usageBtn.querySelector('svg')).toBeTruthy() // Material "memory" icon
    expect(customBtn.innerHTML).toContain('<svg viewBox="0 0 24 24">') // inline SVG passthrough
    expect(remarkableBtn.textContent).not.toContain('📊') // emoji no longer rendered as-is
    expect(remarkableBtn.querySelector('svg')).toBeTruthy() // falls back to the generic glyph
  })

  // quack-extensions#70: extensions now send real Material Symbols names
  // (e.g. "draw", "monitoring") that must render as their own icon, not the
  // generic fallback; a name the local map doesn't know still degrades to
  // the fallback glyph, visibly, with a one-time console warning.
  it('renders a known Material icon name and warns once for an unknown one', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    try {
      render({
        initialExtensions: [
          { name: 'remarkable', title: 'reMarkable', href: '/remarkable/review', icon: 'draw' } as ExtensionInfo,
          { name: 'nope', title: 'Nope', href: '/nope', icon: 'not-a-real-icon' } as ExtensionInfo,
          { name: 'nope2', title: 'Nope2', href: '/nope2', icon: 'not-a-real-icon' } as ExtensionInfo,
        ],
      })
      const nopeBtn = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'Nope')!
      expect(nopeBtn.querySelector('svg')).toBeTruthy() // still renders the fallback glyph, never nothing
      expect(warn).toHaveBeenCalledTimes(1) // once per unknown name, not once per render
      expect(warn.mock.calls[0][0]).toContain('not-a-real-icon')
    } finally {
      warn.mockRestore()
    }
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
