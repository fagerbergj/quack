import type { Meta, StoryObj } from '@storybook/react-vite'
import { ChoicePrompt } from './ChoicePrompt'

const meta: Meta<typeof ChoicePrompt> = {
  title: 'Chat/ChoicePrompt',
  component: ChoicePrompt,
  args: { onSelect: (o: string) => alert(`chose: ${o}`) },
}
export default meta

type Story = StoryObj<typeof ChoicePrompt>

const SPRINGFIELDS = ['Springfield, Illinois', 'Springfield, Missouri', 'Springfield, Massachusetts']

// A clarification with a few discrete options (e.g. which "Springfield").
export const Basic: Story = {
  args: { question: 'Which Springfield do you mean?', options: SPRINGFIELDS },
}

// Disabled while the chosen answer is being sent.
export const Disabled: Story = {
  args: { question: 'Which Springfield do you mean?', options: SPRINGFIELDS, disabled: true },
}

// Resolved: the user has answered; read-only view with the chosen answer.
export const Answered: Story = {
  args: { question: 'Which Springfield do you mean?', options: SPRINGFIELDS, answered: 'Springfield, Missouri' },
}
