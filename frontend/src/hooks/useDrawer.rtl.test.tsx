// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { useDrawer } from './useDrawer'

afterEach(cleanup)

// Owner passes an inline onClose (like NavRail/ChatList both do) - a fresh
// closure identity on every render of the OWNER (e.g. Chat's 5s chat-list
// poll, unrelated to the drawer at all) must not tear down/re-run the
// focus-management effect and steal focus back to the first focusable item.
function Owner({ tick }: { tick: number }) {
  const panelRef = useDrawer(true, () => {})
  return (
    <div>
      <div role="dialog" ref={panelRef}>
        <button>First in panel</button>
        <button>Second in panel</button>
      </div>
      <span data-testid="tick">{tick}</span>
    </div>
  )
}

describe('useDrawer - inline onClose identity churn', () => {
  it('a re-render of the owning component (fresh inline onClose) does not steal focus back', () => {
    const { rerender } = render(<Owner tick={0} />)
    const second = screen.getByRole('button', { name: 'Second in panel' })
    second.focus()
    expect(document.activeElement).toBe(second)

    // Same as a poll/streaming update re-rendering the owner: a brand new
    // inline () => {} is passed to useDrawer as onClose every time.
    rerender(<Owner tick={1} />)
    expect(document.activeElement).toBe(second)
  })
})
