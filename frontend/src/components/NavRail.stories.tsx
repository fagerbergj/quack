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
  args: { route: 'chat', initialCollapsed: false },
  render: args => (
    <div className="h-96">
      <NavRail {...args} />
    </div>
  ),
}

// Memory reachable and highlighted as active - without the overflow menu
// (#746 item 1's core acceptance: Memory is a rail peer of Chats).
export const ExpandedOnMemory: Story = {
  args: { route: 'memory', initialCollapsed: false },
  render: args => (
    <div className="h-96">
      <NavRail {...args} />
    </div>
  ),
}

// Collapsed to icons only - the toggle at the bottom re-expands it.
export const Collapsed: Story = {
  args: { route: 'chat', initialCollapsed: true },
  render: args => (
    <div className="h-96">
      <NavRail {...args} />
    </div>
  ),
}

// Clicking the collapse toggle flips the rail's width and persists the
// choice to localStorage (test case 2: survives a reload).
export const ToggleCollapse: Story = {
  args: { route: 'chat', initialCollapsed: false },
  render: args => (
    <div className="h-96">
      <NavRail {...args} />
    </div>
  ),
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    expect(canvas.getByText('Chats')).toBeInTheDocument()
    await userEvent.click(canvas.getByRole('button', { name: 'Collapse navigation' }))
    expect(canvas.queryByText('Chats')).not.toBeInTheDocument()
    expect(localStorage.getItem('navRailCollapsed')).toBe('1')
  },
}
