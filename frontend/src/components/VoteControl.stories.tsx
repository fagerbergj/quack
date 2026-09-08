import type { Meta, StoryObj } from '@storybook/react-vite'
import { within, userEvent } from 'storybook/test'
import { VoteControl } from './VoteControl'

const meta: Meta<typeof VoteControl> = {
  title: 'Memory/VoteControl',
  component: VoteControl,
  args: { onVote: async () => {} },
}
export default meta

type Story = StoryObj<typeof VoteControl>

export const NoVotes: Story = {
  args: { score: 0 },
}

export const OwnVoteUp: Story = {
  args: { score: 3, ownVote: 'up' },
}

export const OwnVoteDown: Story = {
  args: { score: -1, ownVote: 'down' },
}

// Clicking the currently-active arrow again is the toggle-off gesture (sends
// "none" - see VoteControl.rtl.test.tsx for the assertion on what was sent).
export const ToggleOff: Story = {
  args: { score: 1, ownVote: 'up' },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Upvote' }))
  },
}

// A rejected vote surfaces inline instead of leaving the highlight stuck.
export const VoteFailed: Story = {
  args: {
    score: 0,
    onVote: async () => {
      throw new Error('Request failed')
    },
  },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Upvote' }))
    await canvas.findByText('Vote failed')
  },
}

export const Disabled: Story = {
  args: { score: 2, disabled: true },
}
