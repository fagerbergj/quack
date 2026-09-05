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

// Regression check for the mobile "composer not visible" bug: a phone-sized
// (390x844, #1174's acceptance viewport) frame, clipped with overflow-hidden
// like the real app shell, with the composer pinned to the bottom via flex -
// same layout App.tsx/Chat.tsx use with h-dvh. If the composer's bottom edge
// falls outside this frame, the fix has regressed.
//
// The frame is just a frame: medium:/useCompact() track the real browser
// window, so #1174's compact single-pill composer only appears in a <600px
// window or under DevTools device emulation - this story viewed in a
// >=600px-wide window shows the desktop layout.
export const MobileViewport: Story = {
  args: { disabled: false, streaming: false },
  // fullscreen: this frame IS the simulated device width - the preview's own
  // docs-canvas padding (.storybook/preview.tsx) would otherwise push it
  // past 390px (caught by render-check).
  parameters: { layout: 'fullscreen' },
  decorators: [Story => (
    <div className="w-[390px] h-[844px] mx-auto flex flex-col justify-end overflow-hidden border border-gray-300 dark:border-gray-600 bg-gray-50 dark:bg-gray-900">
      <Story />
    </div>
  )],
}

// Same regression check, streaming: Stop+Queue crowd the row and shrink the
// textarea further than the idle case does - the tightest width the
// placeholder has to fit in (#759 item 2). The two-item queue shows the
// compact "N queued" chip (desktop: the pending bubble rows).
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

// #1174's narrowest acceptance viewport (360x740): the tightest idle check -
// the pill's fixed 44+8+44 row cost leaves the least room for the textarea.
export const MobileViewport360: Story = {
  args: { disabled: false, streaming: false },
  decorators: [Story => (
    <div className="w-[360px] h-[740px] mx-auto flex flex-col justify-end overflow-hidden border border-gray-300 dark:border-gray-600 bg-gray-50 dark:bg-gray-900">
      <Story />
    </div>
  )],
}
