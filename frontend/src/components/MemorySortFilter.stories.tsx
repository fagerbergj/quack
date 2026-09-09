import type { Meta, StoryObj } from '@storybook/react-vite'
import { useState } from 'react'
import { within, userEvent, expect } from 'storybook/test'
import { MemorySortFilter, type MemorySort, type MemorySortFilterProps, type MemoryTierFilter } from './MemorySortFilter'

function Controlled(props: Omit<MemorySortFilterProps, 'sort' | 'onSortChange' | 'bucket' | 'onBucketChange' | 'tier' | 'onTierChange'> & { initialSort?: MemorySort; initialBucket?: string; initialTier?: MemoryTierFilter }) {
  const [sort, setSort] = useState<MemorySort>(props.initialSort ?? 'newest')
  const [bucket, setBucket] = useState(props.initialBucket ?? '')
  const [tier, setTier] = useState<MemoryTierFilter>(props.initialTier ?? '')
  return (
    <MemorySortFilter
      sort={sort}
      onSortChange={setSort}
      bucket={bucket}
      buckets={props.buckets}
      onBucketChange={setBucket}
      tier={tier}
      onTierChange={setTier}
      scopes={props.scopes}
    />
  )
}

const meta: Meta<typeof MemorySortFilter> = {
  title: 'Memory/MemorySortFilter',
  component: MemorySortFilter,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof Controlled>

const BUCKETS = ['repo:NightsOut', 'repo:quack', 'role:research', 'user:jason']

// #746 items 11/15: sort and the bucket filter live in ONE dialog, matching
// the chat sidebar's FilterPanel disclosure pattern - closed by default.
export const Closed: Story = {
  render: () => <Controlled buckets={BUCKETS} />,
}

export const Open: Story = {
  render: () => <Controlled buckets={BUCKETS} />,
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Sort and filter memories' }))
    await canvas.findByRole('dialog')
  },
}

// A non-default sort + an active bucket filter both light up the trigger
// button (border/text turn blue) - the same "active filters" affordance
// FilterPanel uses.
export const WithActiveFilters: Story = {
  render: () => <Controlled buckets={BUCKETS} initialSort="oldest" initialBucket="repo:quack" />,
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Sort and filter memories' }))
    expect(canvas.getByLabelText('Oldest first')).toBeChecked()
    expect(canvas.getByLabelText('Bucket filter')).toHaveValue('repo:quack')
  },
}

// #1267: live/invalidated per bucket, read-only, below the tier filter.
export const WithScopes: Story = {
  render: () => (
    <Controlled
      buckets={BUCKETS}
      scopes={[
        { scope: 'repo:quack', live: 42, invalidated: 5 },
        { scope: 'repo:NightsOut', live: 11, invalidated: 2 },
        { scope: 'user:jason', live: 8, invalidated: 0 },
      ]}
    />
  ),
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Sort and filter memories' }))
    await canvas.findByText('Live / invalidated')
  },
}

// Below `medium` the sort/filter dialog is a bottom sheet with 44px rows.
export const CompactSheet: Story = {
  render: () => <Controlled buckets={BUCKETS} scopes={[{ scope: 'repo:quack', live: 42, invalidated: 5 }]} />,
  parameters: { renderCheck: { viewports: ['mobile', 'desktop'], play: true } },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Sort and filter memories' }))
    await canvas.findByRole('dialog')
  },
}
