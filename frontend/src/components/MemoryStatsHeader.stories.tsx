import type { Meta, StoryObj } from '@storybook/react-vite'
import { within, userEvent } from 'storybook/test'
import { MemoryStatsHeader } from './MemoryStatsHeader'
import type { MemoryWeekStats } from '../api'

const meta: Meta<typeof MemoryStatsHeader> = {
  title: 'Memory/MemoryStatsHeader',
  component: MemoryStatsHeader,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof MemoryStatsHeader>

const WEEKS: MemoryWeekStats[] = [
  { week: '2026-W33', recalls: 40, supported: 20, contradicted: 4, not_relevant: 6, precision: 0.83, support_share: 0.5, minted: 5, invalidated: 1 },
  { week: '2026-W34', recalls: 55, supported: 30, contradicted: 5, not_relevant: 5, precision: 0.86, support_share: 0.55, minted: 8, invalidated: 2 },
  { week: '2026-W35', recalls: 38, supported: 18, contradicted: 8, not_relevant: 4, precision: 0.69, support_share: 0.47, minted: 3, invalidated: 4 },
  { week: '2026-W36', recalls: 61, supported: 40, contradicted: 3, not_relevant: 3, precision: 0.93, support_share: 0.66, minted: 9, invalidated: 0 },
]

export const Populated: Story = {
  args: { weeks: WEEKS, loading: false, error: null },
}

export const Expanded: Story = {
  args: { weeks: WEEKS, loading: false, error: null },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByText('Precision'))
  },
}

// No votes yet this week - a dash, never 0% or NaN.
export const NoVotesYet: Story = {
  args: {
    weeks: [{ week: '2026-W36', recalls: 0, supported: 0, contradicted: 0, not_relevant: 0, precision: 0, support_share: 0, minted: 0, invalidated: 0 }],
    loading: false,
    error: null,
  },
}

export const Empty: Story = {
  args: { weeks: [], loading: false, error: null },
}

export const Loading: Story = {
  args: { weeks: [], loading: true, error: null },
}

export const ErrorState: Story = {
  args: { weeks: [], loading: false, error: 'stats: qdrant unreachable' },
}
