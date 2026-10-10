// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { createElement } from 'react'
import { act, cleanup, render } from '@testing-library/react'
import { Composer } from './Composer'

afterEach(cleanup)

// The placeholder's keyboard hint wraps and clips at phone widths, so it drops below the sm breakpoint.
function mockMatchMedia(matches: boolean) {
  vi.stubGlobal('matchMedia', vi.fn().mockImplementation((query: string) => ({
    matches,
    media: query,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  })))
}

describe('Composer idle placeholder', () => {
  let host: HTMLElement | undefined

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  function mount() {
    host = render(createElement(Composer, { disabled: false, streaming: false, onSubmit: () => {}, onStop: () => {} })).container
  }

  it('keeps the keyboard hint at normal widths', () => {
    mockMatchMedia(false)
    mount()
    const ta = host!.querySelector('textarea')!
    expect(ta.placeholder).toBe('Ask something… (Enter to send, Shift+Enter for newline)')
  })

  it('drops the keyboard hint below the narrow breakpoint', () => {
    mockMatchMedia(true)
    mount()
    const ta = host!.querySelector('textarea')!
    expect(ta.placeholder).toBe('Ask something…')
  })
})

describe('Composer streaming placeholder', () => {
  let host: HTMLElement | undefined

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  function mount() {
    host = render(createElement(Composer, { disabled: false, streaming: true, onSubmit: () => {}, onStop: () => {} })).container
  }

  it('keeps the queuing explanation at normal widths', () => {
    mockMatchMedia(false)
    mount()
    const ta = host!.querySelector('textarea')!
    expect(ta.placeholder).toBe('Type a follow-up… (queues until the current response finishes)')
  })

  it('drops the queuing explanation below the narrow breakpoint, same as the idle placeholder', () => {
    mockMatchMedia(true)
    mount()
    const ta = host!.querySelector('textarea')!
    expect(ta.placeholder).toBe('Type a follow-up…')
  })
})

// Archived must say so distinctly: "Select or start a chat first" is wrong once an archived chat is focused.
describe('Composer archived placeholder', () => {
  let host: HTMLElement | undefined

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  function mount() {
    mockMatchMedia(false)
    host = render(createElement(Composer, { disabled: true, streaming: false, archived: true, onSubmit: () => {}, onStop: () => {} })).container
  }

  it('shows the read-only placeholder instead of the generic disabled one', () => {
    mount()
    const ta = host!.querySelector('textarea')!
    expect(ta.placeholder).toBe('Archived chats are read-only - restore to continue')
  })

  it('disables the textarea', () => {
    mount()
    const ta = host!.querySelector('textarea')!
    expect(ta.disabled).toBe(true)
  })
})

// jsdom reports scrollHeight 0, so tests shadow it. Value goes through the native prototype setter: React's own
// `value` accessor would swallow a plain assignment and the dispatched `input` event would register no change.
describe('Composer auto-grow cap', () => {
  let host: HTMLElement | undefined

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  function mount() {
    host = render(createElement(Composer, { disabled: false, streaming: false, onSubmit: () => {}, onStop: () => {} })).container
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
    mount()
    const ta = host!.querySelector('textarea') as HTMLTextAreaElement
    setLongValue()
    expect(ta.style.height).toBe('128px')
    expect(ta.style.overflowY).toBe('auto')
  })

  it('caps the textarea at 192px at normal widths', () => {
    mockMatchMedia(false)
    mount()
    const ta = host!.querySelector('textarea') as HTMLTextAreaElement
    setLongValue()
    expect(ta.style.height).toBe('192px')
    expect(ta.style.overflowY).toBe('auto')
  })
})
