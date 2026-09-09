import type { Meta, StoryObj } from '@storybook/react-vite'
import { Sheet } from './Sheet'

const meta: Meta<typeof Sheet> = {
  title: 'Chat/Sheet',
  component: Sheet,
  parameters: { layout: 'fullscreen', renderCheck: { viewports: ['mobile', 'desktop'] } },
}
export default meta

type Story = StoryObj<typeof Sheet>

// Bottom sheet below `medium`, centred dialog above it.
export const Centered: Story = {
  args: {
    'aria-label': 'Example',
    className: 'max-w-2xl medium:max-h-[85vh] medium:rounded-2xl bg-gray-50 dark:bg-gray-900 px-5 medium:pb-6 pt-4 space-y-2',
    onClose: () => {},
    children: <p className="text-sm text-gray-700 dark:text-gray-200">Sheet body</p>,
  },
}

// Anchored: a popover beside its trigger at medium+, the same sheet below.
export const Anchored: Story = {
  render: args => (
    <div className="relative inline-block p-6">
      <button className="min-h-[44px] px-3 rounded border border-gray-300 dark:border-gray-600">Trigger</button>
      <Sheet {...args} />
    </div>
  ),
  args: {
    anchored: true,
    role: 'menu',
    className: 'medium:absolute medium:left-6 medium:mt-1 medium:w-44 medium:rounded-lg medium:border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 pt-1 medium:pb-1 text-sm medium:text-xs',
    onClose: () => {},
    children: <button role="menuitem" className="flex w-full min-h-[44px] medium:min-h-0 px-3 py-1.5 text-left text-gray-600 dark:text-gray-300">Menu item</button>,
  },
}
