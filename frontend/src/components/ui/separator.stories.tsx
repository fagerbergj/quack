import type { Meta, StoryObj } from '@storybook/react-vite'
import { Separator } from './separator'

const meta: Meta<typeof Separator> = {
  title: 'UI/Separator',
  component: Separator,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof Separator>

export const Orientations: Story = {
  render: () => (
    <div className="space-y-3 text-sm">
      <p>Above</p>
      <Separator />
      <div className="flex h-6 items-center gap-3">
        <span>One</span>
        <Separator orientation="vertical" />
        <span>Two</span>
      </div>
    </div>
  ),
}
