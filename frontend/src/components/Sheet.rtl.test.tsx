// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { Sheet } from './Sheet'

afterEach(cleanup)

describe('Sheet', () => {
  it('opens as a modal dialog docked to the bottom edge below medium', () => {
    render(<Sheet onClose={() => {}} aria-label="Things"><button>Item</button></Sheet>)
    const dialog = screen.getByRole('dialog', { name: 'Things' })
    expect(dialog.getAttribute('aria-modal')).toBe('true')
    expect(dialog.className).toContain('rounded-t-2xl')
    expect(dialog.parentElement?.className).toContain('items-end')
  })

  it('Escape and a scrim click both close it', () => {
    const onClose = vi.fn()
    render(<Sheet onClose={onClose}><button>Item</button></Sheet>)
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(onClose).toHaveBeenCalledTimes(1)
    fireEvent.click(screen.getByRole('dialog').parentElement!)
    expect(onClose).toHaveBeenCalledTimes(2)
    fireEvent.click(screen.getByRole('button', { name: 'Item' }))
    expect(onClose).toHaveBeenCalledTimes(2)
  })

  it('moves focus in on open and returns it to the opener on close', () => {
    const opener = document.createElement('button')
    document.body.appendChild(opener)
    opener.focus()
    const { unmount } = render(<Sheet onClose={() => {}}><button>Item</button></Sheet>)
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Item' }))
    unmount()
    expect(document.activeElement).toBe(opener)
    opener.remove()
  })

  it('anchored: collapses the scrim at medium+ so the panel can be a popover', () => {
    render(<Sheet anchored role="menu" onClose={() => {}}><button>Item</button></Sheet>)
    const menu = screen.getByRole('menu')
    expect(menu.hasAttribute('aria-modal')).toBe(false)
    expect(menu.parentElement?.className).toContain('medium:contents')
  })
})
