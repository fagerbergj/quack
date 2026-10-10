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
// becomes Queue - a follow-up typed now waits for the run to finish.
export const Streaming: Story = {
  args: { disabled: false, streaming: true },
}

// A run is streaming and the user has already queued follow-ups - shown as
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

// No active chat - input disabled with a hint placeholder.
export const Disabled: Story = {
  args: { disabled: true, streaming: false },
}

// The composer's bottom edge must stay inside a clipped 390x844 frame. Compact layout tracks the real window,
// so the single-pill composer only shows in a <600px window or device emulation.
export const MobileViewport: Story = {
  args: { disabled: false, streaming: false },
  // The preview's docs-canvas padding would otherwise push the frame past 390px.
  parameters: { layout: 'fullscreen' },
  decorators: [Story => (
    <div className="w-[390px] h-[844px] mx-auto flex flex-col justify-end overflow-hidden border border-gray-300 dark:border-gray-600 bg-gray-50 dark:bg-gray-900">
      <Story />
    </div>
  )],
}

// Stop and Queue crowd the row, leaving the placeholder its tightest width.
export const MobileViewportStreaming: Story = {
  args: {
    disabled: false,
    streaming: true,
    queue: [
      { id: '1', text: 'also check the staging build' },
      { id: '2', text: 'and add a changelog entry' },
    ],
  },
  parameters: { layout: 'fullscreen' },
  decorators: [Story => (
    <div className="w-[390px] h-[844px] mx-auto flex flex-col justify-end overflow-hidden border border-gray-300 dark:border-gray-600 bg-gray-50 dark:bg-gray-900">
      <Story />
    </div>
  )],
}

// iOS Safari's keyboard shrinks visualViewport but not dvh; this fakes the short frame App.tsx's
// useVisualViewportHeight produces, so render-check catches the composer sliding off it.
export const MobileViewportKeyboardOpen: Story = {
  args: { disabled: false, streaming: false },
  parameters: { layout: 'fullscreen' },
  decorators: [Story => (
    <div className="w-[390px] h-[420px] mx-auto flex flex-col justify-end overflow-hidden border border-gray-300 dark:border-gray-600 bg-gray-50 dark:bg-gray-900">
      <Story />
    </div>
  )],
}

// Narrowest target viewport: the pill's fixed 44+8+44 row cost leaves the least room for the textarea.
export const MobileViewport360: Story = {
  args: { disabled: false, streaming: false },
  decorators: [Story => (
    <div className="w-[360px] h-[740px] mx-auto flex flex-col justify-end overflow-hidden border border-gray-300 dark:border-gray-600 bg-gray-50 dark:bg-gray-900">
      <Story />
    </div>
  )],
}
