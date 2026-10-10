// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest'
import { createElement } from 'react'
import { act, cleanup, render } from '@testing-library/react'
import { NavRail, displayVersion } from './NavRail'
import { client } from '../generated/client.gen'
import type { ExtensionInfo } from '../api'

afterEach(cleanup)

// Node's fetch/Request (unlike a browser's) refuses to build a Request from a
// relative URL - see MemoryTab.test.ts for the same setup.
client.setConfig({ baseUrl: 'http://localhost' })

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

// The caller owns the open state and nothing is persisted, so these tests drive the open prop and assert
// the stale navRailCollapsed key is neither read nor written.

describe('NavRail', () => {
  let host: HTMLElement | undefined

  beforeEach(() => {
    localStorage.clear()
  })

  afterEach(() => {
    window.history.replaceState(null, '', '/') // undo any /ext/:name navigate() from the click test above
  })

  // open defaults to true so the content tests exercise the drawer body;
  // onClose is a no-op in these unit tests (closing is the App's job).
  function mount(props: Partial<Parameters<typeof NavRail>[0]> = {}) {
    host = render(createElement(NavRail, { route: 'chat', open: true, onClose: () => {}, ...props })).container
  }

  it('shows text labels for both Chats and Memory by default', () => {
    mount()
    expect(host!.textContent).toContain('Chats')
    expect(host!.textContent).toContain('Memory')
  })

  // A stale navRailCollapsed value is neither read (the drawer stays closed) nor rewritten by rendering.
  it('ignores a stale navRailCollapsed localStorage value - neither read nor written', () => {
    localStorage.setItem('navRailCollapsed', '1')
    mount({ open: false })
    expect(host!.textContent).toBe('') // closed renders zero DOM
    expect(host!.querySelector('nav')).toBeNull()
    expect(document.querySelector('[role="dialog"]')).toBeNull()
    expect(localStorage.getItem('navRailCollapsed')).toBe('1') // untouched, never rewritten
  })

  it('marks the active route via aria-current', () => {
    mount({ route: 'memory' })
    const memoryBtn = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'Memory')!
    const chatsBtn = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'Chats')!
    expect(memoryBtn.getAttribute('aria-current')).toBe('page')
    expect(chatsBtn.getAttribute('aria-current')).toBeNull()
  })

  // Extension entries navigate client-side to /ext/:name; a real <a href> would leave the SPA.
  it('renders an extension with a UI descriptor as a client-side /ext/:name nav button', () => {
    mount({ initialExtensions: [{ name: 'remarkable', title: 'reMarkable', href: '/remarkable/review' }] })
    expect(host!.querySelector('a')).toBeNull()
    const btn = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'reMarkable')
    expect(btn).toBeTruthy()
    expect(btn!.textContent).toContain('reMarkable')
    act(() => { btn!.dispatchEvent(new MouseEvent('click', { bubbles: true })) })
    expect(window.location.pathname).toBe('/ext/remarkable')
  })

  // A UI-less extension has nowhere to navigate, so it must not render at all, not even inert.
  it('renders nothing for a UI-less extension (no href)', () => {
    mount({ initialExtensions: [{ name: 'noop' }] })
    expect(host!.textContent).not.toContain('noop')
    expect(host!.querySelector('a')).toBeNull()
    expect(Array.from(host!.querySelectorAll('button')).some(b => b.getAttribute('aria-label') === 'noop')).toBe(false)
  })

  it('renders no extensions section when the only extension has no href', () => {
    mount({ initialExtensions: [{ name: 'github' }] })
    // The extensions divider is the only border-gray-100 element in the drawer
    // panel - absent means no section rendered.
    expect(host!.querySelector('.border-gray-100')).toBeNull()
  })

  it('renders only the href-bearing extension out of a mixed list', () => {
    mount({ initialExtensions: [{ name: 'github' }, { name: 'usage', title: 'Usage', href: '/usage' }] })
    expect(host!.textContent).not.toContain('github')
    const usageBtn = Array.from(host!.querySelectorAll('button')).find(b => b.getAttribute('aria-label') === 'Usage')
    expect(usageBtn).toBeTruthy()
  })

  // A Material icon name renders as that icon and an inline <svg> as-is; anything else, such as a raw
  // emoji, falls back to the generic "extension" glyph.
  it('accepts a Material icon name or inline SVG, and falls back to the generic glyph', () => {
    mount({
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

  // Material Symbols names (e.g. "draw") render as their own icon; an unknown name degrades to the
  // fallback glyph with a one-time console warning.
  it('renders a known Material icon name and warns once for an unknown one', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    try {
      mount({
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
    mount({ initialExtensions: [] })
    expect(host!.querySelector('a')).toBeNull()
  })

  // The footer normalizes whatever the server sends into exactly one "v" prefix, "dev" untouched.
  it('normalizes the version display', () => {
    expect(displayVersion('0.51.26')).toBe('v0.51.26')
    expect(displayVersion('v0.51.26')).toBe('v0.51.26') // no "vv" duplication
    expect(displayVersion('dev')).toBe('dev')
  })

  it('renders the version footer at the bottom, muted, without an icon', () => {
    mount({ versionOverride: '0.51.26' })
    const footer = Array.from(host!.querySelectorAll('span')).find(s => s.textContent === 'v0.51.26')!
    expect(footer).toBeTruthy()
    expect(footer.getAttribute('title')).toBe('0.51.26')
    expect(footer.querySelector('svg')).toBeNull()
  })

  it('renders no version footer when the version is unknown', () => {
    mount({ versionOverride: undefined })
    const versionSpan = Array.from(host!.querySelectorAll('span')).find(s => /^v?\d+\.\d+\.\d+$/.test(s.textContent ?? '') || s.textContent === 'dev')
    expect(versionSpan).toBeUndefined()
  })

  it('fetches GET /api/v1/extensions itself when no seam prop is given', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse([{ name: 'remarkable', title: 'reMarkable', href: '/remarkable/review' }]))
    vi.stubGlobal('fetch', fetchMock)
    try {
      await act(async () => {
        mount()
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
