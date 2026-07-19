// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { CopyButton } from './CopyButton'

describe('CopyButton', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
  })

  it('copies its text to the clipboard on click and flashes a confirmation', () => {
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText } })

    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    act(() => {
      root!.render(createElement(CopyButton, { text: '{"input":1}', label: 'Copy tool call JSON' }))
    })

    const button = host.querySelector('button')!
    expect(button.querySelector('svg')).not.toBeNull() // content-copy glyph, not yet confirmed

    act(() => {
      button.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
    })

    expect(writeText).toHaveBeenCalledWith('{"input":1}')
    expect(button.textContent).toBe('✓')
  })

  it('does not toggle an enclosing <details> when clicked', () => {
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    Object.assign(navigator, { clipboard: { writeText: vi.fn().mockResolvedValue(undefined) } })

    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    act(() => {
      root!.render(
        createElement('details', { open: false }, [
          createElement('summary', { key: 's' }, createElement(CopyButton, { text: 'x' })),
        ]),
      )
    })

    const details = host.querySelector('details')!
    const button = host.querySelector('button')!
    act(() => {
      button.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
    })

    expect(details.open).toBe(false)
  })
})
