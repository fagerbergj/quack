import type { Meta, StoryObj } from '@storybook/react-vite'
import { QuestionBubble } from './QuestionBubble'

const meta: Meta<typeof QuestionBubble> = {
  title: 'Chat/QuestionBubble',
  component: QuestionBubble,
  args: { onSelect: (o: string) => alert(`answered: ${o}`) },
}
export default meta

type Story = StoryObj<typeof QuestionBubble>

const SPRINGFIELDS = ['Springfield, Illinois', 'Springfield, Missouri', 'Springfield, Massachusetts']

// The orchestrator's own get_user_choice clarification: a few discrete options.
export const OrchestratorClarification: Story = {
  args: { agent: 'orchestrator', question: 'Which Springfield do you mean?', options: SPRINGFIELDS },
}

// Disabled while the chosen answer is being sent.
export const Disabled: Story = {
  args: { agent: 'orchestrator', question: 'Which Springfield do you mean?', options: SPRINGFIELDS, disabled: true },
}

// Resolved: the user has answered; read-only view with the chosen answer.
export const Answered: Story = {
  args: { agent: 'orchestrator', question: 'Which Springfield do you mean?', options: SPRINGFIELDS, answered: 'Springfield, Missouri' },
}

// A paused node's mid-node HITL question - no discrete options, free text only,
// credited to the node's own agent rather than the orchestrator. This is what
// used to render as the amber NodeAskPrompt box buried inside the node card; now
// it's a conversation-level bubble like any other question.
export const NodeQuestion: Story = {
  args: { agent: 'web-researcher', question: 'Which time zone should the itinerary use - local or your home time zone?' },
}

export const NodeQuestionAnswered: Story = {
  args: {
    agent: 'web-researcher',
    question: 'Which time zone should the itinerary use - local or your home time zone?',
    answered: 'Local time zone, please.',
  },
}
