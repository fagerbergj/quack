import type { Meta, StoryObj } from '@storybook/react-vite'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuRadioGroup, DropdownMenuRadioItem, DropdownMenuSeparator, DropdownMenuTrigger } from './dropdown-menu'
import { Button } from './button'

const meta: Meta<typeof DropdownMenuContent> = {
  title: 'UI/DropdownMenu',
  component: DropdownMenuContent,
  parameters: { layout: 'padded', renderCheck: { viewports: ['mobile', 'desktop'] } },
}
export default meta

type Story = StoryObj<typeof DropdownMenuContent>

export const Open: Story = {
  render: () => (
    <DropdownMenu defaultOpen modal={false}>
      <DropdownMenuTrigger asChild><Button variant="outline">Actions</Button></DropdownMenuTrigger>
      <DropdownMenuContent align="start">
        <DropdownMenuItem>Download logs</DropdownMenuItem>
        <DropdownMenuItem destructive>Delete</DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuRadioGroup value="system">
          <DropdownMenuRadioItem value="light">Light</DropdownMenuRadioItem>
          <DropdownMenuRadioItem value="system">System</DropdownMenuRadioItem>
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  ),
}
