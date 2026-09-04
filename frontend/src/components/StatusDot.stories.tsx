import type { Meta, StoryObj } from '@storybook/react-vite'
import { StatusDot } from './StatusDot'

const meta: Meta<typeof StatusDot> = {
  title: 'Chat/StatusDot',
  component: StatusDot,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof StatusDot>

// The full set of colored (non-quiet) states, side by side.
export const AllStates: Story = {
  render: () => (
    <div className="flex items-center gap-4">
      {(['running', 'needs_input', 'paused', 'failed', 'cancelled'] as const).map(status => (
        <div key={status} className="flex items-center gap-1.5">
          <StatusDot status={status} />
          <span className="text-xs text-gray-600 dark:text-gray-300">{status}</span>
        </div>
      ))}
    </div>
  ),
}

// idle/done/queued (node variant) render nothing - the common case stays quiet.
export const QuietStates: Story = {
  render: () => (
    <div className="flex items-center gap-4">
      {(['idle', 'done', 'queued'] as const).map(status => (
        <div key={status} className="flex items-center gap-1.5">
          <StatusDot status={status} />
          <span className="text-xs text-gray-600 dark:text-gray-300">{status} (no dot)</span>
        </div>
      ))}
    </div>
  ),
}

// chat variant flags a queued chat (behind max_active_runs); node variant stays quiet.
export const QueuedVariants: Story = {
  render: () => (
    <div className="flex items-center gap-4">
      <div className="flex items-center gap-1.5">
        <StatusDot status="queued" variant="node" />
        <span className="text-xs text-gray-600 dark:text-gray-300">node (quiet)</span>
      </div>
      <div className="flex items-center gap-1.5">
        <StatusDot status="queued" variant="chat" />
        <span className="text-xs text-gray-600 dark:text-gray-300">chat (dot)</span>
      </div>
    </div>
  ),
}

export const Dark: Story = {
  ...AllStates,
  globals: { theme: 'dark' },
}
