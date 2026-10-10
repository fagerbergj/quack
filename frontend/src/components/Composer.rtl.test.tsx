// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Composer } from './Composer'

// `matches` drives both the compact (599px) and narrow-placeholder (639px) queries.
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

// 44x44 is the touch-target floor for the compact pill's icon buttons.
describe('Composer compact pill', () => {
  it('compact send is a 44x44 icon button named via aria-label, not text', () => {
    mockMatchMedia(true)
    render(<Composer disabled={false} streaming={false} onSubmit={() => {}} onStop={() => {}} />)
    const send = screen.getByRole('button', { name: 'Send' })
    expect(send.className).toContain('h-11')
    expect(send.className).toContain('w-11')
    expect(send.getAttribute('aria-label')).toBe('Send')
    // No letters in the visible content, so the accessible name can only come from aria-label.
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
    // Follow-up queueing must stay reachable on mobile.
    const queue = screen.getByRole('button', { name: 'Queue' })
    expect(queue.className).toContain('h-11')
    expect(queue.className).toContain('w-11')
  })

  it('the compact row uses no physical left/right utilities under dir=rtl', () => {
    // jsdom has no layout, so this pins class strings; the visual rtl check is manual.
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
    // A `//` line inside JSX children renders as a text node, not a comment.
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

// The empty /chat route must not read as dead: the first message is what creates the chat.
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

// A rejected onSubmit (e.g. chat create failing) must not erase the draft or leave an unhandled rejection.
describe('Composer restores the draft when onSubmit rejects', () => {
  it('keeps the typed text in the input after a rejected send', async () => {
    mockMatchMedia(false)
    let reject!: (err: Error) => void
    const onSubmit = vi.fn(() => new Promise<void>((_, r) => { reject = r }))
    render(<Composer disabled={false} streaming={false} onSubmit={onSubmit} onStop={() => {}} noChat />)
    const user = userEvent.setup()
    const input = screen.getByPlaceholderText('Ask a question') as HTMLTextAreaElement
    await user.type(input, 'hello{Enter}')
    expect(input.value).toBe('')
    reject(new Error('boom'))
    await waitFor(() => expect(input.value).toBe('hello'))
  })
})
