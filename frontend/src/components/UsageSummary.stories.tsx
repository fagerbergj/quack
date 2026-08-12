import type { Meta, StoryObj } from '@storybook/react-vite'
import { UsageSummary } from './UsageSummary'

const meta: Meta<typeof UsageSummary> = {
  title: 'Chat/UsageSummary',
  component: UsageSummary,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof UsageSummary>

// A plain-reply turn: one model chip.
export const SingleModel: Story = {
  render: () => (
    <UsageSummary
      models={['gpt-oss-120b']}
      usage={{ input_tokens: 4200, output_tokens: 850, reasoning_tokens: 300, cached_tokens: 0, total_tokens: 5350 }}
    />
  ),
}

// A DAG turn: distinct models across its nodes.
export const MultipleModels: Story = {
  render: () => (
    <UsageSummary
      models={['gpt-oss-120b', 'qwen3-coder-next']}
      usage={{ input_tokens: 18000, output_tokens: 3200, reasoning_tokens: 900, cached_tokens: 12000, total_tokens: 22100 }}
    />
  ),
}

// A high cache-hit session - the expandable breakdown's cache rate is the
// headline number here (click/focus the "tok" summary to expand).
export const HighCacheRate: Story = {
  render: () => (
    <UsageSummary
      models={['gpt-oss-120b']}
      usage={{ input_tokens: 50000, output_tokens: 1200, reasoning_tokens: 0, cached_tokens: 45000, total_tokens: 51200 }}
    />
  ),
}

// A fresh chat with no run yet: renders nothing.
export const Empty: Story = {
  render: () => <UsageSummary models={[]} />,
}
