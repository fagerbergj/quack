// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { createElement } from 'react'
import { cleanup, render } from '@testing-library/react'
import { renderToStaticMarkup } from 'react-dom/server'
import { Expandable } from './Expandable'

afterEach(cleanup)

// renderToStaticMarkup runs no layout effects, so `overflows` stays false: this pins the fits-content path.
// The overflow path (fade + Show more) needs a measured DOM and is covered in the stories.
describe('Expandable', () => {
  it('renders children and shows no toggle before measuring (content-fits path)', () => {
    const out = renderToStaticMarkup(
      createElement(Expandable, { children: createElement('p', null, 'hello world') }),
    )
    expect(out).toContain('hello world')
    expect(out).not.toContain('Show more')
    expect(out).not.toContain('overflow-hidden')
  })
})

// Measuring the clamped box ping-pongs into React's max-update-depth guard. The scrollHeight stub reads a
// clamped element under the cap and an unclamped one over, so measuring the clamped box would fail.
describe('Expandable (streaming re-measure)', () => {
  const cap = 100

  it('converges instead of looping when the clamp changes the measured height', () => {
    Object.defineProperty(HTMLElement.prototype, 'scrollHeight', {
      configurable: true,
      get(this: HTMLElement) {
        return this.style.maxHeight ? cap - 10 : cap + 50
      },
    })

    // Re-render the way a streamed answer does: same component, growing content,
    // many commits back to back. Each commit re-measures.
    const el = (i: number) => createElement(Expandable, { maxHeight: cap, children: createElement('p', null, 'token '.repeat(i)) })
    let host!: HTMLElement
    expect(() => {
      const view = render(el(1))
      host = view.container
      for (let i = 2; i <= 60; i++) view.rerender(el(i))
    }).not.toThrow()

    // It settled on "content overflows the cap": clamped, with a Show more toggle.
    expect(host.textContent).toContain('Show more')
    expect(host.querySelector('.overflow-hidden')).not.toBeNull()
  })
})
