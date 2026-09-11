import type { Meta, StoryObj } from '@storybook/react-vite'
import { StatusDot } from './StatusDot'

const meta: Meta<typeof StatusDot> = {
  title: 'Chat/StatusDot',
  component: StatusDot,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof StatusDot>

// Every state at DAG-node size: dot plus the state's name, so no state is
// colour-only and queued/done/idle are visibly distinct.
export const AllStates: Story = {
  render: () => (
    <div className="flex flex-wrap items-center gap-4">
      {(['running', 'needs_input', 'paused', 'failed', 'cancelled', 'queued', 'done', 'idle'] as const).map(status => (
        <StatusDot key={status} status={status} />
      ))}
    </div>
  ),
}

// A DagNode passes a more specific name for the paused family.
export const CustomLabel: Story = {
  render: () => <StatusDot status="needs_input" label="needs your answer" />,
}

// Chat variant: a bare dot that stays quiet for idle/done, so a long chat
// list only flags what needs attention.
export const ChatVariant: Story = {
  render: () => (
    <div className="flex items-center gap-4">
      {(['running', 'needs_input', 'failed', 'done', 'idle'] as const).map(status => (
        <div key={status} className="flex items-center gap-1.5">
          <StatusDot status={status} variant="chat" />
          <span className="text-xs text-gray-600 dark:text-gray-300">{status}{status === 'done' || status === 'idle' ? ' (no dot)' : ''}</span>
        </div>
      ))}
    </div>
  ),
}

export const Dark: Story = {
  ...AllStates,
  globals: { theme: 'dark' },
}
