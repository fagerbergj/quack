import type { Meta, StoryObj } from '@storybook/react-vite'
import { within, userEvent, expect } from 'storybook/test'
import { NavRail } from './NavRail'

const meta: Meta<typeof NavRail> = {
  title: 'Chat/NavRail',
  component: NavRail,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof NavRail>

// Default: text labels beside each icon, Chats highlighted as the active route.
export const ExpandedOnChats: Story = {
  args: { route: 'chat', initialCollapsed: false, initialExtensions: [] },
  render: args => (
    <div className="h-96">
      <NavRail {...args} />
    </div>
  ),
}

// Memory reachable and highlighted as active - without the overflow menu
// (#746 item 1's core acceptance: Memory is a rail peer of Chats).
export const ExpandedOnMemory: Story = {
  args: { route: 'memory', initialCollapsed: false, initialExtensions: [] },
  render: args => (
    <div className="h-96">
      <NavRail {...args} />
    </div>
  ),
}

// Fully collapsed (#759 item 1): no rail, no icons - just the restore
// button, a real focusable control with an accessible name, not a hover
// zone. The 320px frame stands in for a narrow phone viewport: at that
// width the restore button is the entire footprint the nav costs.
export const Collapsed: Story = {
  args: { route: 'chat', initialCollapsed: true, initialExtensions: [] },
  render: args => (
    <div className="h-96 w-[320px] relative border border-dashed border-gray-300 dark:border-gray-600">
      <NavRail {...args} />
    </div>
  ),
}

// Extension nav entries (#/api/v1/extensions): a module with a UI descriptor
// is a real link, one without stays name-only and inert.
export const WithExtensions: Story = {
  args: {
    route: 'chat',
    initialCollapsed: false,
    initialExtensions: [
      { name: 'remarkable', title: 'reMarkable', href: '/remarkable/review' },
      { name: 'noop' },
    ],
  },
  render: args => (
    <div className="h-96">
      <NavRail {...args} />
    </div>
  ),
}

// Clicking the collapse toggle removes the rail entirely and persists the
// choice to localStorage (test case 1: survives a reload).
export const ToggleCollapse: Story = {
  args: { route: 'chat', initialCollapsed: false, initialExtensions: [] },
  render: args => (
    <div className="h-96 relative">
      <NavRail {...args} />
    </div>
  ),
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    expect(canvas.getByText('Chats')).toBeInTheDocument()
    await userEvent.click(canvas.getByRole('button', { name: 'Collapse navigation' }))
    expect(canvas.queryByText('Chats')).not.toBeInTheDocument()
    expect(canvas.getByRole('button', { name: 'Expand navigation' })).toBeInTheDocument()
    expect(localStorage.getItem('navRailCollapsed')).toBe('1')
  },
}
