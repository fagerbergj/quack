// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { renderToStaticMarkup } from 'react-dom/server'
import { Expandable } from './Expandable'

// renderToStaticMarkup doesn't run layout effects, so the measured `overflows`
// state is false — the initial (server) render shows the children uncapped with
// NO toggle and NO clamp. This pins the fits-content path; the overflow path
// (fade + Show more) is measured-DOM behaviour, covered visually in the stories.
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

// Regression: the collapse decision must not depend on the collapse itself.
//
// Collapsing changes the box's own layout — `overflow-hidden` + `max-height`
// makes it a block formatting context and drops its full height from the
// scrolling ancestor — so a clamped box does not necessarily measure the same
// as an unclamped one. If the component measures the box it clamps, the
// decision feeds back into its own input: measure tall → clamp → measure short
// → unclamp → measure tall → … Re-measuring on every commit (a layout effect
// with no dependency array) turns that into an unbounded chain of nested
// updates, and a streaming turn — hundreds of token renders — walks straight
// into React's guard: "Maximum update depth exceeded" (#185), after which the
// chat tree renders nothing at all.
//
// jsdom has no layout, so scrollHeight is stubbed to model exactly that: a
// clamped element (one carrying an inline max-height) measures under the cap,
// an unclamped one over it. Whatever the component measures, it must never be
// the element it clamps.
describe('Expandable (streaming re-measure)', () => {
  const cap = 100
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
  })

  it('converges instead of looping when the clamp changes the measured height', () => {
    Object.defineProperty(HTMLElement.prototype, 'scrollHeight', {
      configurable: true,
      get(this: HTMLElement) {
        return this.style.maxHeight ? cap - 10 : cap + 50
      },
    })
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true

    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)

    // Re-render the way a streamed answer does: same component, growing content,
    // many commits back to back. Each commit re-measures.
    expect(() => {
      for (let i = 1; i <= 60; i++) {
        act(() => {
          root!.render(
            createElement(Expandable, {
              maxHeight: cap,
              children: createElement('p', null, 'token '.repeat(i)),
            }),
          )
        })
      }
    }).not.toThrow()

    // It settled on "content overflows the cap": clamped, with a Show more toggle.
    expect(host.textContent).toContain('Show more')
    expect(host.querySelector('.overflow-hidden')).not.toBeNull()
  })
})
