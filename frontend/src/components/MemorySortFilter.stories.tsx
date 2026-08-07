import type { Meta, StoryObj } from '@storybook/react-vite'
import { useState } from 'react'
import { within, userEvent, expect } from 'storybook/test'
import { MemorySortFilter, type MemorySort, type MemorySortFilterProps } from './MemorySortFilter'

function Controlled(props: Omit<MemorySortFilterProps, 'sort' | 'onSortChange' | 'bucket' | 'onBucketChange'> & { initialSort?: MemorySort; initialBucket?: string }) {
  const [sort, setSort] = useState<MemorySort>(props.initialSort ?? 'newest')
  const [bucket, setBucket] = useState(props.initialBucket ?? '')
  return (
    <MemorySortFilter
      sort={sort}
      onSortChange={setSort}
      bucket={bucket}
      buckets={props.buckets}
      onBucketChange={setBucket}
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
