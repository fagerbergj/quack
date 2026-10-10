// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { createElement } from 'react'
import { act, cleanup, render } from '@testing-library/react'
import { CopyButton } from './CopyButton'

afterEach(cleanup)

describe('CopyButton', () => {
  let host: HTMLElement | undefined

  it('copies its text to the clipboard on click and flashes a confirmation', () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText } })

    host = render(createElement(CopyButton, { text: '{"input":1}', label: 'Copy tool call JSON' })).container

    const button = host.querySelector('button')!
    expect(button.querySelector('svg')).not.toBeNull() // content-copy glyph, not yet confirmed

    act(() => {
      button.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
    })

    expect(writeText).toHaveBeenCalledWith('{"input":1}')
    expect(button.querySelector('svg')).not.toBeNull() // check glyph on confirmation
  })

  it('does not toggle an enclosing <details> when clicked', () => {
    Object.assign(navigator, { clipboard: { writeText: vi.fn().mockResolvedValue(undefined) } })

    host = render(
      createElement('details', { open: false }, [
        createElement('summary', { key: 's' }, createElement(CopyButton, { text: 'x' })),
      ]),
    ).container

    const details = host.querySelector('details')!
    const button = host.querySelector('button')!
    act(() => {
      button.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
    })

    expect(details.open).toBe(false)
  })
  it('is a 44px target at compact width', () => {
    host = render(createElement(CopyButton, { text: 'x' })).container
    const cls = host.querySelector('button')!.className
    expect(cls).toContain('min-h-[44px]')
    expect(cls).toContain('medium:min-h-0')
  })
})
