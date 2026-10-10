// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { Sheet } from './Sheet'

afterEach(cleanup)

describe('Sheet', () => {
  it('opens as a labelled modal dialog docked to the bottom edge below medium', () => {
    render(<Sheet onClose={() => {}} aria-label="Things"><button>Item</button></Sheet>)
    const dialog = screen.getByRole('dialog', { name: 'Things' })
    expect(dialog.tagName).toBe('DIALOG')
    expect(dialog.className).toContain('mt-auto')
    expect(dialog.className).toContain('medium:m-auto')
  })

  it('a backdrop click closes it; a click inside does not', () => {
    const onClose = vi.fn()
    render(<Sheet onClose={onClose} aria-label="Things"><button>Item</button></Sheet>)
    fireEvent.click(screen.getByRole('button', { name: 'Item' }))
    expect(onClose).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('dialog'))
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('returns focus to the opener when unmounted while open', () => {
    const opener = document.createElement('button')
    document.body.appendChild(opener)
    opener.focus()
    const { unmount } = render(<Sheet onClose={() => {}} aria-label="Things"><button>Item</button></Sheet>)
    screen.getByRole('button', { name: 'Item' }).focus()
    unmount()
    expect(document.activeElement).toBe(opener)
    opener.remove()
  })

  // jsdom has no matchMedia, so this renders the desktop (medium+) mode.
  it('anchored: a non-modal popover menu below its trigger at medium+, closed by light dismiss', () => {
    const onClose = vi.fn()
    render(<Sheet anchored="right" role="menu" aria-label="Actions" onClose={onClose}><button>Item</button></Sheet>)
    const dialog = screen.getByRole('dialog', { name: 'Actions' })
    expect(dialog.getAttribute('popover')).toBe('auto')
    expect(dialog.querySelector('[role="menu"]')).not.toBeNull()
    expect(dialog.className).toContain('medium:[position-area:bottom_span-left]')
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Item' }))
    dialog.hidePopover()
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('unanchored: always a modal, never a popover', () => {
    render(<Sheet onClose={() => {}} aria-label="Things"><button>Item</button></Sheet>)
    expect(screen.getByRole('dialog', { name: 'Things' }).hasAttribute('popover')).toBe(false)
  })
})
