// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Composer } from './Composer'

// jsdom has no matchMedia - stub it the way Composer.test.ts does; the
// matches flag drives both useCompact() ((max-width: 599px)) and the
// narrow-viewport placeholder ((max-width: 639px)).
function mockMatchMedia(narrow: boolean) {
  vi.stubGlobal('matchMedia', vi.fn().mockImplementation((query: string) => ({
    matches: narrow,
    media: query,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  })))
}

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

// #1174: below the 600px medium size class the composer is one compact
// pill - 44x44 icon buttons (NN/g's touch-target floor), no full-text
// action buttons, no physical left/right utilities in the row.
describe('Composer compact pill', () => {
  it('compact send is a 44x44 icon button named via aria-label, not text', () => {
    mockMatchMedia(true)
    render(<Composer disabled={false} streaming={false} onSubmit={() => {}} onStop={() => {}} />)
    const send = screen.getByRole('button', { name: 'Send' })
    expect(send.className).toContain('h-11')
    expect(send.className).toContain('w-11')
    expect(send.getAttribute('aria-label')).toBe('Send')
    // The visible content is the decorative glyph only - no letters, so the
    // accessible name above can only come from aria-label (owner rule: no
    // full-text action buttons where an icon does).
    expect(send.textContent ?? '').not.toMatch(/\p{L}/u)
  })

  it('compact send submits the typed text', async () => {
    mockMatchMedia(true)
    const onSubmit = vi.fn()
    render(<Composer disabled={false} streaming={false} onSubmit={onSubmit} onStop={() => {}} />)
    const user = userEvent.setup()
    await user.type(screen.getByRole('textbox'), 'hello')
    await user.click(screen.getByRole('button', { name: 'Send' }))
    expect(onSubmit).toHaveBeenCalledWith('hello', [], [])
  })

  it('while streaming, the row shows a 44x44 icon Stop and Queue stays reachable', () => {
    mockMatchMedia(true)
    render(<Composer disabled={false} streaming={true} onSubmit={() => {}} onStop={() => {}} />)
    const stop = screen.getByRole('button', { name: 'Stop' })
    expect(stop.className).toContain('h-11')
    expect(stop.className).toContain('w-11')
    expect(stop.className).toContain('rounded-full')
    // Follow-up queueing must stay reachable on mobile - the queued chip
    // depends on it.
    const queue = screen.getByRole('button', { name: 'Queue' })
    expect(queue.className).toContain('h-11')
    expect(queue.className).toContain('w-11')
  })

  it('the compact row uses no physical left/right utilities under dir=rtl', () => {
    // jsdom computes no layout - this pins the class strings; the visual
    // dir=rtl check is the manual browser step.
    mockMatchMedia(true)
    render(
      <div dir="rtl">
        <Composer disabled={false} streaming={true} onSubmit={() => {}} onStop={() => {}} />
      </div>,
    )
    const stop = screen.getByRole('button', { name: 'Stop' })
    const send = screen.getByRole('button', { name: 'Queue' })
    const attach = screen.getByRole('button', { name: 'Attach file' })
    const row = send.parentElement
    expect(row).not.toBeNull()
    for (const util of ['left-', 'right-', 'ml-', 'mr-']) {
      expect(row!.className).not.toContain(util)
    }
    // Both icon buttons keep their accessible names under rtl.
    expect(stop).toBeTruthy()
    expect(attach).toBeTruthy()
    expect(send.getAttribute('aria-label')).toBe('Queue')
  })

  it('renders no stray text from a bare JS comment above the compact row', () => {
    // Regression: a `//` line inside JSX children isn't a comment, it's a
    // text node - catches it without pinning the wrapper's pixel height.
    mockMatchMedia(true)
    const { container } = render(<Composer disabled={false} streaming={false} onSubmit={() => {}} onStop={() => {}} />)
    expect(container.textContent ?? '').not.toContain('//')
  })

  it('tapping the "N queued" chip expands the queued messages with an always-visible remove', async () => {
    mockMatchMedia(true)
    const onRemoveQueued = vi.fn()
    render(
      <Composer
        disabled={false}
        streaming={true}
        onSubmit={() => {}}
        onStop={() => {}}
        queue={[
          { id: 'q1', text: 'also check the staging build' },
          { id: 'q2', text: 'and add a changelog entry' },
        ]}
        onRemoveQueued={onRemoveQueued}
      />,
    )
    const chip = screen.getByText('2 queued')
    const details = chip.closest('details')
    expect(details).not.toBeNull()
    expect(details!.open).toBe(false)
    // <details> is DOM-handled: tapping the summary opens the chip.
    const user = userEvent.setup()
    await user.click(chip)
    expect(details!.open).toBe(true)
    expect(screen.getByText('also check the staging build')).toBeTruthy()
    // No hover on touch - every bubble's remove control is visible.
    const removes = screen.getAllByRole('button', { name: 'Remove queued message' })
    expect(removes).toHaveLength(2)
    await user.click(removes[0])
    expect(onRemoveQueued).toHaveBeenCalledWith('q1')
  })
})

// Audit finding 8: the empty /chat route (no chat selected) must not read as
// dead - the composer stays enabled and invites the first message, which is
// what creates the chat (Chat.tsx's submitMessage).
describe('Composer noChat (empty /chat route, audit finding 8)', () => {
  it('stays enabled with an "Ask a question" placeholder, not the old disabled copy', () => {
    mockMatchMedia(false)
    render(<Composer disabled={false} streaming={false} onSubmit={() => {}} onStop={() => {}} noChat />)
    const input = screen.getByPlaceholderText('Ask a question') as HTMLTextAreaElement
    expect(input.disabled).toBe(false)
    expect(screen.queryByPlaceholderText('Select or start a chat first')).toBeNull()
  })

  it('submits normally - the caller (Chat.tsx), not the Composer, creates the chat', async () => {
    mockMatchMedia(false)
    const onSubmit = vi.fn()
    render(<Composer disabled={false} streaming={false} onSubmit={onSubmit} onStop={() => {}} noChat />)
    const user = userEvent.setup()
    await user.type(screen.getByPlaceholderText('Ask a question'), 'hello{Enter}')
    expect(onSubmit).toHaveBeenCalledWith('hello', [], [])
  })
})

// Review finding on #1350: onSubmit used to be fired-and-forgotten, so a
// rejection (e.g. the empty-route chat create failing) both erased the
// draft and left an unhandled rejection. submit() must restore it instead.
describe('Composer restores the draft when onSubmit rejects', () => {
  it('keeps the typed text in the input after a rejected send', async () => {
    mockMatchMedia(false)
    let reject!: (err: Error) => void
    const onSubmit = vi.fn(() => new Promise<void>((_, r) => { reject = r }))
    render(<Composer disabled={false} streaming={false} onSubmit={onSubmit} onStop={() => {}} noChat />)
    const user = userEvent.setup()
    const input = screen.getByPlaceholderText('Ask a question') as HTMLTextAreaElement
    await user.type(input, 'hello{Enter}')
    // Cleared immediately for a responsive send...
    expect(input.value).toBe('')
    reject(new Error('boom'))
    // ...then restored once the send actually failed.
    await waitFor(() => expect(input.value).toBe('hello'))
  })
})
