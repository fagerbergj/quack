import type { Meta, StoryObj } from '@storybook/react-vite'
import { Composer } from './Composer'

const meta: Meta<typeof Composer> = {
  title: 'Chat/Composer',
  component: Composer,
  args: {
    onSubmit: (text: string) => alert(`submit: ${text}`),
    onStop: () => alert('stop'),
    onRemoveQueued: (id: string) => alert(`remove queued: ${id}`),
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

// While a turn streams: input stays live, Stop cancels the run, and Send
// becomes Queue — a follow-up typed now waits for the run to finish.
export const Streaming: Story = {
  args: { disabled: false, streaming: true },
}

// A run is streaming and the user has already queued follow-ups — shown as
// pending rows above the composer, in send order, each removable.
export const StreamingWithQueue: Story = {
  args: {
    disabled: false,
    streaming: true,
    queue: [
      { id: '1', text: 'also check the staging build' },
      { id: '2', text: 'and add a changelog entry' },
    ],
  },
}

// No active chat — input disabled with a hint placeholder.
export const Disabled: Story = {
  args: { disabled: true, streaming: false },
}
