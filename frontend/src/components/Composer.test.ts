// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { Composer } from './Composer'

// #759 item 2: the keyboard hint in the idle placeholder wraps to a second
// line that gets clipped at phone widths - dropped below Tailwind's sm
// breakpoint (matchMedia, not the story's container width).
function mockMatchMedia(matches: boolean) {
  vi.stubGlobal('matchMedia', vi.fn().mockImplementation((query: string) => ({
    matches,
    media: query,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  })))
}

describe('Composer idle placeholder', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
    vi.unstubAllGlobals()
  })

  function render() {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    act(() => {
      root!.render(createElement(Composer, { disabled: false, streaming: false, onSubmit: () => {}, onStop: () => {} }))
    })
  }

  it('keeps the keyboard hint at normal widths', () => {
    mockMatchMedia(false)
    render()
    const ta = host!.querySelector('textarea')!
    expect(ta.placeholder).toBe('Ask something… (Enter to send, Shift+Enter for newline)')
  })

  it('drops the keyboard hint below the narrow breakpoint', () => {
    mockMatchMedia(true)
    render()
    const ta = host!.querySelector('textarea')!
    expect(ta.placeholder).toBe('Ask something…')
  })
})

describe('Composer streaming placeholder', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
    vi.unstubAllGlobals()
  })

  function render() {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    act(() => {
      root!.render(createElement(Composer, { disabled: false, streaming: true, onSubmit: () => {}, onStop: () => {} }))
    })
  }

  it('keeps the queuing explanation at normal widths', () => {
    mockMatchMedia(false)
    render()
    const ta = host!.querySelector('textarea')!
    expect(ta.placeholder).toBe('Type a follow-up… (queues until the current response finishes)')
  })

  it('drops the queuing explanation below the narrow breakpoint, same as the idle placeholder', () => {
    mockMatchMedia(true)
    render()
    const ta = host!.querySelector('textarea')!
    expect(ta.placeholder).toBe('Type a follow-up…')
  })
})
