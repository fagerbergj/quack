// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { GitHubLink } from './GitHubLink'

afterEach(cleanup)

const URL = 'https://github.com/acme/widgets/issues/7'

describe('GitHubLink', () => {
  it('opens the issue in a new tab without leaking the opener, labelled with the repo', () => {
    render(<GitHubLink url={URL} repo="acme/widgets" />)
    const link = screen.getByRole('link', { name: 'Open acme/widgets on GitHub' })
    expect(link.getAttribute('href')).toBe(URL)
    expect(link.getAttribute('target')).toBe('_blank')
    expect(link.getAttribute('rel')).toBe('noopener noreferrer')
    expect(screen.getByText('acme/widgets')).toBeTruthy()
  })

  it('falls back to a generic label and no repo span without a repo', () => {
    render(<GitHubLink url={URL} />)
    const link = screen.getByRole('link', { name: 'Open on GitHub' })
    expect(link.getAttribute('href')).toBe(URL)
    expect(link.textContent).toBe('↗')
  })
})
