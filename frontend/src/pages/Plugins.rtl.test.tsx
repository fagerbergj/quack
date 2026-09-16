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

describe('Plugins row error rendering', () => {
  const longError = 'git clone: fatal: unable to access \'https://github.com/acme/broken.git/\': Could not resolve host: github.com\nfatal: clone of \'https://github.com/acme/broken.git\' into submodule path \'plugins/broken/repo\' failed\nretry 3/3 failed after 12.4s, giving up'

  it('renders the full multi-line clone error instead of truncating it', () => {
    renderRow({ name: 'broken', entry: 'github:acme/broken', source: 'github', error: longError })
    const el = screen.getByText((_, node) => node?.tagName === 'SPAN' && node.textContent === longError)
    expect(el.className).not.toContain('truncate')
    expect(el.className).toContain('whitespace-pre-line')
  })

  it('keeps the update-check-failed message reachable without hover, via a native details disclosure', () => {
    renderRow(
      { name: 'flaky', entry: 'github:acme/flaky', source: 'github', installed_sha: 'a'.repeat(40) },
      [{ name: 'flaky', behind: false, error: 'update check: dial tcp: lookup github.com: i/o timeout' }],
    )
    const summary = screen.getByText('check failed')
    expect(summary.closest('details')).not.toBeNull()
    // Present in the DOM (reachable by tapping the disclosure) even though
    // <details> without `open` hides it visually - textContent still finds it.
    expect(screen.getByText('update check: dial tcp: lookup github.com: i/o timeout')).toBeTruthy()
  })
})
