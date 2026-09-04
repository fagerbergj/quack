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

// An archived chat's Composer is disabled like "no chat selected", but must say
// so distinctly - "Select or start a chat first" is actively wrong once a real,
// archived chat is focused (part of the "focused archived chat appears active" fix).
describe('Composer archived placeholder', () => {
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
    mockMatchMedia(false)
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    act(() => {
      root!.render(createElement(Composer, { disabled: true, streaming: false, archived: true, onSubmit: () => {}, onStop: () => {} }))
    })
  }

  it('shows the read-only placeholder instead of the generic disabled one', () => {
    render()
    const ta = host!.querySelector('textarea')!
    expect(ta.placeholder).toBe('Archived chats are read-only - restore to continue')
  })

  it('disables the textarea', () => {
    render()
    const ta = host!.querySelector('textarea')!
    expect(ta.disabled).toBe(true)
  })
})

// #1174: the auto-grow cap follows the width - 128px (max-h-32) compact,
// 192px (max-h-48) desktop - so both must be pinned. jsdom reports
// scrollHeight 0 (no layout), so each test shadows it on the instance. The
// value is set through the native prototype setter, because React 19
// defines its own `value` accessor on controlled nodes that keeps the value
// tracker in step - a plain assignment would register no change to the
// dispatched `input` event (the same trick user-event uses internally).
describe('Composer auto-grow cap', () => {
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

  function setLongValue() {
    const ta = host!.querySelector('textarea') as HTMLTextAreaElement
    Object.defineProperty(ta, 'scrollHeight', { value: 300, configurable: true, writable: true })
    const nativeSet = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!
    act(() => {
      nativeSet.call(ta, 'a '.repeat(60).trim())
      ta.dispatchEvent(new Event('input', { bubbles: true }))
    })
  }

  it('caps the textarea at 128px when compact', () => {
    mockMatchMedia(true)
    render()
    const ta = host!.querySelector('textarea') as HTMLTextAreaElement
    setLongValue()
    expect(ta.style.height).toBe('128px')
    expect(ta.style.overflowY).toBe('auto')
  })

  it('caps the textarea at 192px at normal widths', () => {
    mockMatchMedia(false)
    render()
    const ta = host!.querySelector('textarea') as HTMLTextAreaElement
    setLongValue()
    expect(ta.style.height).toBe('192px')
    expect(ta.style.overflowY).toBe('auto')
  })
})
