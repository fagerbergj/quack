// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from 'vitest'
import { cleanup, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AttachmentPreviews } from './AttachmentUI'

afterEach(cleanup)

// jsdom doesn't implement <dialog>.showModal - stub it so the click handler
// doesn't throw; behaviour under test is which element renders, not the
// browser's own modal mechanics.
function stubDialog() {
  HTMLDialogElement.prototype.showModal = vi.fn(function (this: HTMLDialogElement) { this.setAttribute('open', '') })
  HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) { this.removeAttribute('open') })
}

// #1138: an image attachment renders as a real <img> thumbnail (click to
// view full size), never the old collapsed "N attachment(s)" text-only chip.
describe('AttachmentPreviews', () => {
  it('renders an <img> with alt text for an image attachment', () => {
    stubDialog()
    render(<AttachmentPreviews previews={[{ url: 'blob:1', mime: 'image/png', name: 'cat.png' }]} />)
    const img = screen.getByRole('img', { name: 'cat.png' })
    expect(img).toBeTruthy()
    expect(img.getAttribute('src')).toBe('blob:1')
  })

  it('opens the full-size view on click, and the Close button closes it', async () => {
    stubDialog()
    const user = userEvent.setup()
    render(<AttachmentPreviews previews={[{ url: 'blob:1', mime: 'image/png', name: 'cat.png' }]} />)
    await user.click(screen.getByRole('button', { name: 'View cat.png full size' }))
    expect(HTMLDialogElement.prototype.showModal).toHaveBeenCalled()

    const dialog = document.querySelector('dialog')!
    expect(dialog.hasAttribute('open')).toBe(true)
    await user.click(screen.getByRole('button', { name: 'Close' }))
    expect(dialog.hasAttribute('open')).toBe(false)
  })

  it('a backdrop click closes the dialog; clicking the image itself does not', async () => {
    stubDialog()
    const user = userEvent.setup()
    render(<AttachmentPreviews previews={[{ url: 'blob:1', mime: 'image/png', name: 'cat.png' }]} />)
    await user.click(screen.getByRole('button', { name: 'View cat.png full size' }))
    const dialog = document.querySelector('dialog')!
    dialog.setAttribute('open', '') // stubbed showModal already does this; explicit for clarity

    // Clicking the full-size image (a child of the dialog) must not close it.
    await user.click(within(dialog).getByRole('img', { name: 'cat.png' }))
    expect(dialog.hasAttribute('open')).toBe(true)

    // Clicking the dialog's own backdrop area (the <dialog> element itself) closes it.
    await user.click(dialog)
    expect(dialog.hasAttribute('open')).toBe(false)
  })

  it('renders a text chip (no <img>) for a non-image attachment', () => {
    stubDialog()
    render(<AttachmentPreviews previews={[{ url: 'blob:2', mime: 'audio/wav', name: 'clip.wav' }]} />)
    expect(screen.queryByRole('img')).toBeNull()
    expect(screen.getByText('clip.wav')).toBeTruthy()
  })

  it('renders nothing for an empty list', () => {
    const { container } = render(<AttachmentPreviews previews={[]} />)
    expect(container.firstChild).toBeNull()
  })
})
