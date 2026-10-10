// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { createElement } from 'react'
import { act, cleanup, render } from '@testing-library/react'
import { ChatMenu } from './ChatMenu'

afterEach(cleanup)

// Download Logs lives in the overflow menu as a plain link to the recording endpoint.
describe('ChatMenu', () => {
  let host: HTMLElement | undefined

  beforeEach(() => {
    // jsdom has no matchMedia; ChatMenu's theme picker (useTheme) calls it on mount.
    vi.stubGlobal('matchMedia', vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })))
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  function mount() {
    host = render(createElement(ChatMenu, { chatId: 'chat-1' })).container
  }

  it('hides the menu until the ⋯ trigger is clicked', () => {
    mount()
    expect(host!.textContent).not.toContain('Download Logs')
    const trigger = host!.querySelector('button[aria-label="Chat actions"]')!
    act(() => trigger.dispatchEvent(new MouseEvent('click', { bubbles: true })))
    expect(host!.textContent).toContain('Download Logs')
  })

  it('the Download Logs entry is a plain link to the recording endpoint, unchanged except label/placement', () => {
    mount()
    const trigger = host!.querySelector('button[aria-label="Chat actions"]')!
    act(() => trigger.dispatchEvent(new MouseEvent('click', { bubbles: true })))
    const link = host!.querySelector('a[role="menuitem"]') as HTMLAnchorElement
    expect(link.getAttribute('href')).toBe('/api/v1/chats/chat-1/recording')
  })
})
