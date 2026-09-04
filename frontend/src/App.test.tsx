// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import App from './App'
import { ChatStoreProvider } from './state/ChatStoreProvider'
import { client } from './generated/client.gen'

// Node's fetch/Request (unlike a browser's) refuses to build a Request from a
// relative URL - see NavRail.test.ts for the same setup.
client.setConfig({ baseUrl: 'http://localhost' })

const extensions = [{ name: 'usage', title: 'Usage', href: '/usage' }]

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

// jsdom has no matchMedia; App's theme init and the media-query hooks call it
// on mount.
function mockMatchMedia() {
  vi.stubGlobal('matchMedia', vi.fn().mockImplementation((query: string) => ({
    matches: false,
    media: query,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  })))
}

// Every route App can render fetches on mount - answer each endpoint with an
// empty-but-valid body so the pages settle without a server.
function stubFetch() {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    // The generated client calls fetch with a Request, and String() of a
    // Request is "[object Request]" in Node - go through .url instead.
    const url = input instanceof URL ? input.href : typeof input === 'string' ? input : (input as Request).url
    if (url.includes('/api/v1/chats')) return jsonResponse({ data: [] })
    if (url.includes('/api/v1/memories')) return jsonResponse({ memories: [], total: 0 })
    if (url.includes('/api/v1/extensions')) return jsonResponse(extensions)
    if (url.includes('/api/v1/config')) return jsonResponse({})
    return jsonResponse({}, 404)
  })
  vi.stubGlobal('fetch', fetchMock)
}

// #1171: the nav drawer's open state lives in App (always false on load,
// never persisted) and the NavToggle in each page's header leading slot
// drives it - so open-on-click and the close paths are tested here, where
// the state lives, while NavRail's own suites test the drawer body.
describe('App nav drawer', () => {
  beforeEach(() => {
    localStorage.clear()
    mockMatchMedia()
    stubFetch()
    window.history.replaceState(null, '', '/chat')
  })

  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  function renderAt(path: string) {
    window.history.replaceState(null, '', path)
    render(<ChatStoreProvider><App /></ChatStoreProvider>)
  }

  it('is closed on a fresh load - even with a stale navRailCollapsed value in localStorage', () => {
    localStorage.setItem('navRailCollapsed', '1')
    renderAt('/chat')
    expect(screen.queryByRole('dialog', { name: 'Main navigation' })).toBeNull()
    expect(localStorage.getItem('navRailCollapsed')).toBe('1') // the key is no longer read or rewritten
  })

  it('carries the toggle on all three routes', async () => {
    renderAt('/chat')
    expect(screen.getByRole('button', { name: 'Toggle navigation' })).toBeTruthy()
    cleanup()
    renderAt('/memory')
    expect(screen.getByRole('button', { name: 'Toggle navigation' })).toBeTruthy()
    cleanup()
    renderAt('/ext/usage')
    expect(screen.getByRole('button', { name: 'Toggle navigation' })).toBeTruthy()
    // The extension's own UI lands when the extensions fetch resolves.
    await waitFor(() => expect(document.querySelector('iframe')).toBeTruthy())
  })

  // #1175: the rail's hamburger duplicated the chat-list toggle's glyph. With
  // the rail's column gone, the chat-list ☰ is the only one in the DOM.
  it('has exactly one ☰ in the DOM - the chat-list toggle', () => {
    renderAt('/chat')
    const glyphHolders = Array.from(document.querySelectorAll('*')).filter(el => el.textContent?.trim() === '☰')
    expect(glyphHolders).toHaveLength(1)
    expect(glyphHolders[0].tagName).toBe('BUTTON')
    expect(glyphHolders[0].getAttribute('aria-label')).toBe('Toggle chat list')
  })

  it('opens the drawer on toggle click, with focus moving into the panel', async () => {
    const user = userEvent.setup()
    renderAt('/chat')
    const trigger = screen.getByRole('button', { name: 'Toggle navigation' })
    expect(trigger.getAttribute('aria-expanded')).toBe('false')
    await user.click(trigger)

    const dialog = await screen.findByRole('dialog', { name: 'Main navigation' })
    expect(dialog).toBeTruthy()
    expect(trigger.getAttribute('aria-expanded')).toBe('true')
    expect(screen.getByRole('button', { name: 'Chats' })).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Memory' })).toBeTruthy()
    // useDrawer moves focus into the panel on open - its first focusable is
    // the ✕ close button.
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Close navigation' }))
  })

  it('closes on item selection and navigates to the picked route', async () => {
    const user = userEvent.setup()
    renderAt('/chat')
    await user.click(screen.getByRole('button', { name: 'Toggle navigation' }))
    await screen.findByRole('dialog', { name: 'Main navigation' })

    await user.click(screen.getByRole('button', { name: 'Memory' }))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Main navigation' })).toBeNull())
    expect(window.location.pathname).toBe('/memory')
  })

  it('closes on Esc and returns focus to the toggle', async () => {
    const user = userEvent.setup()
    renderAt('/chat')
    const trigger = screen.getByRole('button', { name: 'Toggle navigation' })
    await user.click(trigger)
    await screen.findByRole('dialog', { name: 'Main navigation' })

    await user.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Main navigation' })).toBeNull())
    expect(document.activeElement).toBe(trigger)
  })

  it('closes on a backdrop tap', async () => {
    const user = userEvent.setup()
    renderAt('/chat')
    await user.click(screen.getByRole('button', { name: 'Toggle navigation' }))
    await screen.findByRole('dialog', { name: 'Main navigation' })

    const backdrop = document.querySelector('.bg-black\\/50')
    expect(backdrop).not.toBeNull()
    await user.click(backdrop!)
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Main navigation' })).toBeNull())
  })
})
