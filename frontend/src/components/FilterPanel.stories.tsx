import type { Meta, StoryObj } from '@storybook/react-vite'
import { useState } from 'react'
import { within, userEvent } from 'storybook/test'
import { FilterPanel, type Facet, type FilterPanelProps } from './FilterPanel'

const FACETS: Facet[] = [
  {
    key: 'origin',
    label: 'Origin',
    options: [
      { value: 'direct', label: 'Direct', count: 5 },
      { value: 'github', label: 'GitHub', count: 3 },
    ],
  },
  {
    key: 'status',
    label: 'Status',
    options: [
      { value: 'running', label: 'Running', count: 1 },
      { value: 'failed', label: 'Failed', count: 1 },
      { value: 'idle', label: 'Idle', count: 6 },
    ],
  },
  {
    key: 'repo',
    label: 'Repo',
    options: [
      { value: 'fagerbergj/quack', label: 'fagerbergj/quack', count: 2 },
      { value: 'fagerbergj/games', label: 'fagerbergj/games', count: 1 },
    ],
  },
  {
    key: 'type',
    label: 'Type',
    options: [
      { value: 'issue', label: 'Issue', count: 2 },
      { value: 'pr', label: 'PR', count: 1 },
    ],
  },
]

// Stateful wrapper: stories need onToggle/onClear to actually mutate
// `selected` so the interactive states (open, active filters) render for real.
function Controlled(props: Omit<FilterPanelProps, 'selected' | 'onToggle' | 'onClear'> & { initial?: Record<string, string[]> }) {
  const [selected, setSelected] = useState<Record<string, string[]>>(props.initial ?? {})
  return (
    <FilterPanel
      facets={props.facets}
      selected={selected}
      onToggle={(key, value) =>
        setSelected(prev => {
          const current = prev[key] ?? []
          const next = current.includes(value) ? current.filter(v => v !== value) : [...current, value]
          return { ...prev, [key]: next }
        })
      }
      onClear={() => setSelected({})}
    />
  )
}

const meta: Meta<typeof FilterPanel> = {
  title: 'Chat/FilterPanel',
  component: FilterPanel,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof Controlled>

// Closed by default: just the funnel icon, no active-filter badge.
export const Closed: Story = {
  render: () => <Controlled facets={FACETS} />,
}

// Click the funnel to see the facet groups (eBay-style checklists with counts).
export const Open: Story = {
  render: () => <Controlled facets={FACETS} />,
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Filter chats' }))
  },
}

// A few filters already active: the funnel shows a count badge, and "Clear all"
// appears in the popover header.
export const WithActiveFilters: Story = {
  render: () => <Controlled facets={FACETS} initial={{ origin: ['github'], status: ['running', 'failed'] }} />,
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Filter chats' }))
  },
}

export const NoFacets: Story = {
  render: () => <Controlled facets={[]} />,
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Filter chats' }))
  },
}
