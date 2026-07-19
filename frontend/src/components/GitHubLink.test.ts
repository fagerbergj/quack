import { describe, it, expect } from 'vitest'
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { GitHubLink } from './GitHubLink'

describe('GitHubLink', () => {
  it('links to the given URL and shows the ↗ glyph', () => {
    const out = renderToStaticMarkup(createElement(GitHubLink, { url: 'https://github.com/acme/widgets/issues/7' }))
    expect(out).toContain('href="https://github.com/acme/widgets/issues/7"')
    expect(out).toContain('target="_blank"')
    expect(out).toContain('↗')
  })

  it('renders the repo name when given one', () => {
    const out = renderToStaticMarkup(
      createElement(GitHubLink, { url: 'https://github.com/acme/widgets/issues/7', repo: 'acme/widgets' }),
    )
    expect(out).toContain('acme/widgets')
    expect(out).toContain('Open acme/widgets on GitHub')
  })

  it('omits the repo label and uses a generic aria-label when no repo is given', () => {
    const out = renderToStaticMarkup(createElement(GitHubLink, { url: 'https://github.com/acme/widgets/issues/7' }))
    expect(out).not.toContain('<span class="truncate">')
    expect(out).toContain('Open on GitHub')
  })
})
