import type { Meta, StoryObj } from '@storybook/react-vite'
import { ChoicePrompt } from './ChoicePrompt'

const meta: Meta<typeof ChoicePrompt> = {
  title: 'Chat/ChoicePrompt',
  component: ChoicePrompt,
  args: { onSelect: (o: string) => alert(`chose: ${o}`) },
}
export default meta

type Story = StoryObj<typeof ChoicePrompt>

// A clarification with a few discrete options (e.g. which "Springfield").
export const Basic: Story = {
  args: { options: ['Springfield, Illinois', 'Springfield, Missouri', 'Springfield, Massachusetts'] },
}

// Disabled while the chosen answer is being sent.
export const Disabled: Story = {
  args: { options: ['Yes', 'No'], disabled: true },
}
