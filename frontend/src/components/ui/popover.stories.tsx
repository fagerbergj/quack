import type { Meta, StoryObj } from '@storybook/react-vite'
import { Popover, PopoverContent, PopoverTrigger } from './popover'
import { Button } from './button'

const meta: Meta<typeof PopoverContent> = {
  title: 'UI/Popover',
  component: PopoverContent,
  parameters: { layout: 'padded', renderCheck: { viewports: ['mobile', 'desktop'] } },
}
export default meta

type Story = StoryObj<typeof PopoverContent>

export const Open: Story = {
  render: () => (
    <Popover defaultOpen>
      <PopoverTrigger asChild><Button variant="outline">Filter</Button></PopoverTrigger>
      <PopoverContent className="w-56">Popover body</PopoverContent>
    </Popover>
  ),
}
