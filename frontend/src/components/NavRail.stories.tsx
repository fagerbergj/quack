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

// The trigger (NavToggle) lives in each page header, so these stories drive the open prop directly.
// The fixed inset-0 overlay floats over the Storybook frame, which stands in for the app.

// Chats highlighted as the active route; Memory is a peer, not in an overflow menu.
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

// Closed renders nothing at all; the 360px frame is the narrowest target device.
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

// The canonical drawer story at phone width.
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

// A module with an href navigates client-side to /ext/:name; one without renders nothing at all
// (see the absent 'github' entry).
export const WithExtensions: Story = {
  args: {
    route: 'chat',
    open: true,
    initialExtensions: [
      { name: 'remarkable', title: 'reMarkable', href: '/remarkable/review', icon: 'draw' },
      { name: 'usage', title: 'Usage', href: '/usage', icon: 'monitoring' },
      { name: 'legacy', title: 'Legacy', href: '/legacy', icon: '📊' },
      { name: 'github' },
    ],
  },
  render: args => (
    <div className="h-96">
      <NavRail {...args} />
    </div>
  ),
}

// The version footer is "v" prefixed once regardless of what the server sends.
export const WithVersion: Story = {
  args: { route: 'chat', open: true, initialExtensions: [], versionOverride: '0.51.26' },
  render: args => (
    <div className="h-96 w-[360px] relative border border-dashed border-gray-300 dark:border-gray-600">
      <NavRail {...args} />
    </div>
  ),
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    const version = canvas.getByTitle('0.51.26')
    expect(version).toBeInTheDocument()
    expect(version).toHaveTextContent('v0.51.26')
  },
}
