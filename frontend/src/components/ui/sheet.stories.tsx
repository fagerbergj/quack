import type { Meta, StoryObj } from '@storybook/react-vite'
import { Sheet, SheetContent } from './sheet'

const meta: Meta<typeof SheetContent> = {
  title: 'UI/Sheet',
  component: SheetContent,
  parameters: { layout: 'fullscreen', renderCheck: { viewports: ['mobile', 'desktop'] } },
}
export default meta

type Story = StoryObj<typeof SheetContent>

export const LeftDrawer: Story = {
  render: () => (
    <Sheet open>
      <SheetContent title="Navigation" className="p-4 text-sm">Drawer body</SheetContent>
    </Sheet>
  ),
}
