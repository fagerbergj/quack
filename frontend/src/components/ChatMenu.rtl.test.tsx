// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ChatMenu } from './ChatMenu'

// jsdom has no matchMedia; ChatMenu's theme picker (useTheme) calls it on
// mount - stub per-test like App.test.tsx/Composer.rtl.test.tsx, not at
// module scope.
function mockMatchMedia() {
  vi.stubGlobal('matchMedia', vi.fn().mockImplementation((query: string) => ({
    matches: false,
    media: query,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  })))
}

beforeEach(() => {
  localStorage.clear()
  document.documentElement.classList.remove('dark')
  mockMatchMedia()
})
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

// #1136: the header hides its inline token/model UsageSummary below the
// medium (600px) size class - this is the escape hatch, always available
// through the same kebab regardless of width.
describe('ChatMenu usage row', () => {
  it('shows the token/model summary in the menu when a usage prop is given', async () => {
    const user = userEvent.setup()
    render(<ChatMenu chatId="c1" usage={{ models: ['gpt-5'], usage: { total_tokens: 5522 } }} />)
    await user.click(screen.getByRole('button', { name: 'Chat actions' }))
    expect(screen.getByText('gpt-5')).toBeTruthy()
    expect(screen.getByText('5,522 tok')).toBeTruthy()
  })

  it('omits the usage row when no usage prop is given', async () => {
    const user = userEvent.setup()
    render(<ChatMenu chatId="c1" />)
    await user.click(screen.getByRole('button', { name: 'Chat actions' }))
    expect(screen.queryByText(/tok$/)).toBeNull()
  })

  it('the trigger button meets the 44px touch-target floor', () => {
    render(<ChatMenu chatId="c1" />)
    const btn = screen.getByRole('button', { name: 'Chat actions' })
    expect(btn.className).toContain('min-w-[44px]')
    expect(btn.className).toContain('min-h-[44px]')
  })
})

// #1173: the kebab's Light/Dark/System entries are the only in-app theme
// control.
describe('ChatMenu theme picker', () => {
  it('switches to Dark and persists it to localStorage', async () => {
    const user = userEvent.setup()
    render(<ChatMenu chatId="c1" />)
    await user.click(screen.getByRole('button', { name: 'Chat actions' }))
    await user.click(screen.getByRole('menuitemradio', { name: /Dark/ }))
    expect(localStorage.getItem('theme')).toBe('dark')
    expect(document.documentElement.classList.contains('dark')).toBe(true)
  })

  it('marks the current choice with aria-checked', async () => {
    localStorage.setItem('theme', 'light')
    const user = userEvent.setup()
    render(<ChatMenu chatId="c1" />)
    await user.click(screen.getByRole('button', { name: 'Chat actions' }))
    expect(screen.getByRole('menuitemradio', { name: /Light/ }).getAttribute('aria-checked')).toBe('true')
    expect(screen.getByRole('menuitemradio', { name: /Dark/ }).getAttribute('aria-checked')).toBe('false')
  })

  it('leaves the menu open after selecting a theme (APG menuitemradio pattern)', async () => {
    const user = userEvent.setup()
    render(<ChatMenu chatId="c1" />)
    await user.click(screen.getByRole('button', { name: 'Chat actions' }))
    await user.click(screen.getByRole('menuitemradio', { name: /Dark/ }))
    expect(screen.getByRole('menu')).toBeTruthy()
    expect(screen.getByRole('menuitemradio', { name: /Dark/ }).getAttribute('aria-checked')).toBe('true')
  })
})
