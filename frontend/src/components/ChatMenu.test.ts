// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { createElement } from 'react'
import { act, cleanup, render } from '@testing-library/react'
import { ChatMenu } from './ChatMenu'

afterEach(cleanup)

// Download Logs lives in the overflow menu as a plain link to the recording endpoint.
describe('ChatMenu', () => {
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
    render(createElement(ChatMenu, { chatId: 'chat-1' }))
  }

  it('hides the menu until the ⋯ trigger is pressed', () => {
    mount()
    expect(document.body.textContent).not.toContain('Download Logs')
    const trigger = document.body.querySelector('button[aria-label="Chat actions"]')!
    act(() => { trigger.dispatchEvent(new MouseEvent('pointerdown', { bubbles: true, button: 0 })) })
    expect(document.body.textContent).toContain('Download Logs')
  })

  it('the Download Logs entry is a plain link to the recording endpoint, unchanged except label/placement', () => {
    mount()
    const trigger = document.body.querySelector('button[aria-label="Chat actions"]')!
    act(() => { trigger.dispatchEvent(new MouseEvent('pointerdown', { bubbles: true, button: 0 })) })
    const link = document.body.querySelector('a[role="menuitem"]') as HTMLAnchorElement
    expect(link.getAttribute('href')).toBe('/api/v1/chats/chat-1/recording')
  })
})
