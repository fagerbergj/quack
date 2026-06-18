import type { Meta, StoryObj } from '@storybook/react-vite'
import { Composer } from './Composer'

const meta: Meta<typeof Composer> = {
  title: 'Chat/Composer',
  component: Composer,
  args: {
    onSubmit: (text: string) => alert(`submit: ${text}`),
    onStop: () => alert('stop'),
  },
  // Pin to the bottom like the real layout so the textarea growth reads correctly.
  decorators: [Story => <div className="h-64 flex flex-col justify-end bg-gray-50 dark:bg-gray-900"><Story /></div>],
}
export default meta

type Story = StoryObj<typeof Composer>

// Ready for input with an active chat.
export const Empty: Story = {
  args: { disabled: false, streaming: false },
}

// While a turn streams: Send becomes Stop and the input is locked.
export const Streaming: Story = {
  args: { disabled: false, streaming: true },
}

// No active chat — input disabled with a hint placeholder.
export const Disabled: Story = {
  args: { disabled: true, streaming: false },
}
