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

// Fully collapsed (#759 item 1, revised #870): a slim ~40px icon strip, not
// an empty sliver - Chats/Memory/extensions and the expand chevron are all
// still real, focusable, accessible-named buttons. The 320px frame stands in
// for a narrow phone viewport.
export const Collapsed: Story = {
  args: { route: 'chat', initialCollapsed: true, initialExtensions: [] },
  render: args => (
    <div className="h-96 w-[320px] relative border border-dashed border-gray-300 dark:border-gray-600">
      <NavRail {...args} />
    </div>
  ),
}

// Collapsed with extensions: the icon strip carries an extension's icon too
// (🧩 fallback, or the API-provided icon - see WithExtensions below).
export const CollapsedWithExtensions: Story = {
  args: {
    route: 'chat',
    initialCollapsed: true,
    initialExtensions: [
      { name: 'remarkable', title: 'reMarkable', href: '/remarkable/review' },
      { name: 'usage', title: 'Usage', href: '/usage' },
    ],
  },
  render: args => (
    <div className="h-96 w-[320px] relative border border-dashed border-gray-300 dark:border-gray-600">
      <NavRail {...args} />
    </div>
  ),
}

// Extension nav entries (#/api/v1/extensions): a module with a UI descriptor
// (href) gets a nav entry that navigates client-side to this app's own
// /ext/:name host page (#870) - one without an href renders nothing at all,
// not an inert placeholder (see the 'github' entry here, which is absent).
export const WithExtensions: Story = {
  args: {
    route: 'chat',
    initialCollapsed: false,
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

// Compact (<600px, #1131/#1133): the persistent rail is replaced by a 44px
// hamburger - a visible entry point, not a hidden-only menu - at a 360px
// frame (our narrowest target device).
export const CompactClosed: Story = {
  args: { route: 'chat', initialExtensions: [], forceCompact: true },
  render: args => (
    <div className="h-96 w-[360px] relative border border-dashed border-gray-300 dark:border-gray-600">
      <NavRail {...args} />
    </div>
  ),
}

// Tapping the hamburger opens the off-canvas drawer with the same
// Chats/Memory list the expanded rail shows.
export const CompactOpen: Story = {
  args: { route: 'chat', initialExtensions: [], forceCompact: true },
  render: args => (
    <div className="h-96 w-[360px] relative border border-dashed border-gray-300 dark:border-gray-600">
      <NavRail {...args} />
    </div>
  ),
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Open navigation' }))
    expect(canvas.getByRole('dialog', { name: 'Main navigation' })).toBeInTheDocument()
    expect(canvas.getByRole('button', { name: 'Chats' })).toBeInTheDocument()
  },
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
