// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach } from 'vitest'
import { act, cleanup, render, screen } from '@testing-library/react'
import { useTheme } from './useTheme'

afterEach(cleanup)

// Fake matchMedia: one shared MQL per query string, with a listener list so
// tests can flip `matches` and fire `change` like a real OS toggle would.
let mql: { matches: boolean; listeners: Set<() => void> }
beforeEach(() => {
  localStorage.clear()
  document.documentElement.classList.remove('dark')
  mql = { matches: false, listeners: new Set() }
  window.matchMedia = (() => ({
    get matches() { return mql.matches },
    addEventListener: (_: string, cb: () => void) => mql.listeners.add(cb),
    removeEventListener: (_: string, cb: () => void) => mql.listeners.delete(cb),
  })) as unknown as typeof window.matchMedia
})

function fireOsChange(next: boolean) {
  mql.matches = next
  mql.listeners.forEach(cb => cb())
}

function Probe() {
  const [theme, setTheme] = useTheme()
  return (
    <div>
      <span data-testid="theme">{theme}</span>
      <button onClick={() => setTheme('dark')}>set-dark</button>
      <button onClick={() => setTheme('light')}>set-light</button>
      <button onClick={() => setTheme('system')}>set-system</button>
    </div>
  )
}

describe('useTheme', () => {
  it('system mode follows a fake matchMedia change after mount', () => {
    render(<Probe />)
    expect(document.documentElement.classList.contains('dark')).toBe(false)
    act(() => fireOsChange(true))
    expect(document.documentElement.classList.contains('dark')).toBe(true)
    expect(document.documentElement.style.colorScheme).toBe('dark')
  })

  it('an explicit dark choice survives a later system change', () => {
    localStorage.setItem('theme', 'dark')
    render(<Probe />)
    expect(document.documentElement.classList.contains('dark')).toBe(true)
    act(() => fireOsChange(true))
    expect(document.documentElement.classList.contains('dark')).toBe(true)
    act(() => fireOsChange(false))
    expect(document.documentElement.classList.contains('dark')).toBe(true)
  })

  it('an explicit light choice stays light while the OS is dark', () => {
    localStorage.setItem('theme', 'light')
    render(<Probe />)
    expect(document.documentElement.classList.contains('dark')).toBe(false)
    expect(document.documentElement.style.colorScheme).toBe('light')
    act(() => fireOsChange(true))
    expect(document.documentElement.classList.contains('dark')).toBe(false)
    expect(document.documentElement.style.colorScheme).toBe('light')
  })

  it('switching from dark to light removes the dark class', () => {
    localStorage.setItem('theme', 'dark')
    render(<Probe />)
    expect(document.documentElement.classList.contains('dark')).toBe(true)
    act(() => screen.getByText('set-light').click())
    expect(document.documentElement.classList.contains('dark')).toBe(false)
    expect(document.documentElement.style.colorScheme).toBe('light')
  })

  it('choosing System removes the stored theme key', () => {
    localStorage.setItem('theme', 'dark')
    render(<Probe />)
    act(() => screen.getByText('set-system').click())
    expect(localStorage.getItem('theme')).toBeNull()
  })
})
