// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { ChatMenu } from './ChatMenu'

// jsdom has no matchMedia; ChatMenu's theme picker (useTheme) calls it on mount.
vi.stubGlobal('matchMedia', vi.fn().mockImplementation((query: string) => ({
  matches: false,
  media: query,
  addEventListener: vi.fn(),
  removeEventListener: vi.fn(),
})))

// #746 items 2/3: Download Logs moved from a standing header link into the
// chat header's ⋯ overflow menu, relabelled but still a plain link to the
// same recording endpoint.
describe('ChatMenu', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
  })

  function render() {
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    act(() => {
      root!.render(createElement(ChatMenu, { chatId: 'chat-1' }))
    })
  }

  it('hides the menu until the ⋯ trigger is clicked', () => {
    render()
    expect(host!.textContent).not.toContain('Download Logs')
    const trigger = host!.querySelector('button[aria-label="Chat actions"]')!
    act(() => trigger.dispatchEvent(new MouseEvent('click', { bubbles: true })))
    expect(host!.textContent).toContain('Download Logs')
  })

  it('the Download Logs entry is a plain link to the recording endpoint, unchanged except label/placement', () => {
    render()
    const trigger = host!.querySelector('button[aria-label="Chat actions"]')!
    act(() => trigger.dispatchEvent(new MouseEvent('click', { bubbles: true })))
    const link = host!.querySelector('a[role="menuitem"]') as HTMLAnchorElement
    expect(link.getAttribute('href')).toBe('/api/v1/chats/chat-1/recording')
  })
})
