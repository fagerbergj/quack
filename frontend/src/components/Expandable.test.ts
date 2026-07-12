import { describe, it, expect } from 'vitest'
import { createElement } from 'react'
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
