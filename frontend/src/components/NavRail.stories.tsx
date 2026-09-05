import type { Meta, StoryObj } from '@storybook/react-vite'
import { within, expect } from 'storybook/test'
import { NavRail } from './NavRail'

const meta: Meta<typeof NavRail> = {
  title: 'Chat/NavRail',
  component: NavRail,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof NavRail>

// #1171: NavRail is a pure overlay drawer at every width - open mounts the
// fixed panel, closed renders nothing. The trigger lives in each page's
// header leading slot (NavToggle), not in this component, so these stories
// drive the drawer through its open prop. The fixed inset-0 overlay floats
// over the Storybook frame, which stands in for the app.

// Chats highlighted as the active route (#746 item 1: Memory is a peer of
// Chats, no overflow menu).
export const OpenOnChats: Story = {
  args: { route: 'chat', open: true, initialExtensions: [] },
  render: args => (
    <div className="h-96">
      <NavRail {...args} />
    </div>
  ),
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    expect(canvas.getByRole('dialog', { name: 'Main navigation' })).toBeInTheDocument()
    expect(canvas.getByRole('button', { name: 'Chats' })).toBeInTheDocument()
    expect(canvas.getByRole('button', { name: 'Memory' })).toBeInTheDocument()
  },
}

// Memory highlighted as active.
export const OpenOnMemory: Story = {
  args: { route: 'memory', open: true, initialExtensions: [] },
  render: args => (
    <div className="h-96">
      <NavRail {...args} />
    </div>
  ),
}

// Closed renders nothing at all - no rail, no strip, no hamburger column
// (this is what #1171 removes; the 360px frame is the narrowest target
// device).
export const Closed: Story = {
  args: { route: 'chat', open: false, initialExtensions: [] },
  render: args => (
    <div className="h-96 w-[360px] relative border border-dashed border-gray-300 dark:border-gray-600">
      <NavRail {...args} />
    </div>
  ),
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    expect(canvas.queryByRole('dialog')).not.toBeInTheDocument()
    expect(canvas.queryByText('Chats')).not.toBeInTheDocument()
  },
}

// The canonical drawer story at phone width (#1145's sub-600px shape, now
// the only shape, at every width).
export const Open: Story = {
  args: { route: 'chat', open: true, initialExtensions: [] },
  render: args => (
    <div className="h-96 w-[360px] relative border border-dashed border-gray-300 dark:border-gray-600">
      <NavRail {...args} />
    </div>
  ),
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    expect(canvas.getByRole('dialog', { name: 'Main navigation' })).toBeInTheDocument()
    expect(canvas.getByRole('button', { name: 'Chats' })).toBeInTheDocument()
  },
}

// Extension nav entries (#/api/v1/extensions): a module with a UI descriptor
// (href) gets a nav entry that navigates client-side to this app's own
// /ext/:name host page (#870) - one without an href renders nothing at all,
// not an inert placeholder (see the 'github' entry here, which is absent).
export const WithExtensions: Story = {
  args: {
    route: 'chat',
    open: true,
    initialExtensions: [
      { name: 'remarkable', title: 'reMarkable', href: '/remarkable/review' },
      { name: 'usage', title: 'Usage', href: '/usage', icon: '📊' },
      { name: 'github' },
    ],
  },
  render: args => (
    <div className="h-96">
      <NavRail {...args} />
    </div>
  ),
}
