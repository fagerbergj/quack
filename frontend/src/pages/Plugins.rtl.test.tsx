// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import Plugins from './Plugins'
import type { Plugin, PluginUpdate } from '../api'

afterEach(cleanup)

function renderRow(plugin: Plugin, updates: PluginUpdate[] = []) {
  render(<Plugins navOpen={false} onToggleNav={() => {}} initialPlugins={[plugin]} initialUpdates={updates} />)
}

describe('Plugins row ref label', () => {
  it('labels the embedded row instead of falling back to "default branch"', () => {
    renderRow({ name: 'quack', entry: '', source: 'embedded' })
    expect(screen.getByText('bundled with quack')).toBeTruthy()
    expect(screen.queryByText('default branch')).toBeNull()
  })

  it('shows nothing for a local row, which has no branch either', () => {
    renderRow({ name: 'skills', entry: '/data/skills', source: 'local' })
    expect(screen.queryByText('default branch')).toBeNull()
    expect(screen.queryByText('bundled with quack')).toBeNull()
  })

  it('shows "default branch" only for a github row with no pinned ref', () => {
    renderRow({ name: 'dotagents', entry: 'github:fagerbergj/dotagents', source: 'github' })
    expect(screen.getByText('default branch')).toBeTruthy()
  })

  it('shows the pinned ref for a github row instead of "default branch"', () => {
    renderRow({ name: 'ponytail', entry: 'github:fagerbergj/ponytail@v1.4', source: 'github', ref: 'v1.4' })
    expect(screen.getByText('v1.4')).toBeTruthy()
    expect(screen.queryByText('default branch')).toBeNull()
  })
})

describe('Plugins touch targets', () => {
  it('keeps the row Update and Remove buttons at the 44px floor', () => {
    renderRow({ name: 'dotagents', entry: 'github:fagerbergj/dotagents', source: 'github' })
    for (const name of ['Update dotagents', 'Remove dotagents']) {
      const cls = screen.getByRole('button', { name }).className
      expect(cls).toContain('w-11')
      expect(cls).toContain('h-11')
    }
  })

  it('keeps the Add button at the 44px floor', () => {
    render(<Plugins navOpen={false} onToggleNav={() => {}} initialPlugins={[]} initialUpdates={[]} />)
    expect(screen.getByRole('button', { name: 'Add' }).className).toContain('min-h-[44px]')
  })
})

describe('Plugins row error rendering', () => {
  const longError = 'git clone: fatal: unable to access \'https://github.com/acme/broken.git/\': Could not resolve host: github.com\nfatal: clone of \'https://github.com/acme/broken.git\' into submodule path \'plugins/broken/repo\' failed\nretry 3/3 failed after 12.4s, giving up'

  it('renders the full multi-line clone error instead of truncating it', () => {
    renderRow({ name: 'broken', entry: 'github:acme/broken', source: 'github', error: longError })
    const el = screen.getByText((_, node) => node?.tagName === 'SPAN' && node.textContent === longError)
    expect(el.className).not.toContain('truncate')
    expect(el.className).toContain('whitespace-pre-line')
    // Load-bearing: a flex item's default min-width:auto ignores break-word's
    // wrap points, so an unbroken URL/path would overflow the row without this.
    expect(el.className).toContain('min-w-0')
  })

  it('opens the update-check-failed message on a tap, not just hover', () => {
    renderRow(
      { name: 'flaky', entry: 'github:acme/flaky', source: 'github', installed_sha: 'a'.repeat(40) },
      [{ name: 'flaky', behind: false, error: 'update check: dial tcp: lookup github.com: i/o timeout' }],
    )
    const summary = screen.getByText('check failed')
    const details = summary.closest('details') as HTMLDetailsElement
    expect(details.open).toBe(false)
    summary.click()
    expect(details.open).toBe(true)
  })
})
