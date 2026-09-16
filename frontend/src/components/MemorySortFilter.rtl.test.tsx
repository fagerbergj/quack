// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { cleanup, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemorySortFilter } from './MemorySortFilter'

afterEach(cleanup)

// The per-scope diagnostics row (map callback) is only exercised through the
// popover: one <li> per scope, each carrying its own counts.
describe('MemorySortFilter scope diagnostics', () => {
  it('renders a row per scope with its diagnostic counts', async () => {
    render(
      <MemorySortFilter
        sort="newest"
        onSortChange={() => {}}
        bucket=""
        buckets={['repo:quack']}
        onBucketChange={() => {}}
        tier=""
        onTierChange={() => {}}
        scopes={[
          { scope: 'repo:quack', live: 42, invalidated: 5, never_recalled: 20, no_votes: 15, unsupported_verified: 3 },
          { scope: 'user:jason', live: 8, invalidated: 0, never_recalled: 1, no_votes: 2, unsupported_verified: 0 },
        ]}
      />,
    )
    await userEvent.click(screen.getByRole('button', { name: 'Sort and filter memories' }))

    const dialog = screen.getByRole('dialog', { name: 'Sort and filter memories' })
    const rows = within(dialog).getAllByRole('listitem')
    expect(rows).toHaveLength(2)
    within(rows[0]).getByText('repo:quack')
    within(rows[0]).getByText('20 never recalled')
    within(rows[1]).getByText('user:jason')
    within(rows[1]).getByText('0 unsupported verified')
  })

  it('omits the diagnostics section entirely when scopes is empty', async () => {
    render(
      <MemorySortFilter
        sort="newest"
        onSortChange={() => {}}
        bucket=""
        buckets={[]}
        onBucketChange={() => {}}
        tier=""
        onTierChange={() => {}}
        scopes={[]}
      />,
    )
    await userEvent.click(screen.getByRole('button', { name: 'Sort and filter memories' }))
    expect(screen.queryByText('Live / invalidated')).toBeNull()
  })
})
