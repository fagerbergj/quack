// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { NavRail } from './NavRail'

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
})
