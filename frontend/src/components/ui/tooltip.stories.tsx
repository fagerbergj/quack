import type { Meta, StoryObj } from '@storybook/react-vite'
import { Tooltip } from './tooltip'
import { Button } from './button'
import { Icon } from '../Icon'

const meta: Meta<typeof Tooltip> = {
  title: 'UI/Tooltip',
  component: Tooltip,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof Tooltip>

// Hover or keyboard-focus the button; the aria-label, not the tooltip, names it.
export const OnIconButton: Story = {
  render: () => (
    <Tooltip content="Refresh artifacts">
      <Button size="icon" variant="ghost" aria-label="Refresh artifacts"><Icon name="refresh" className="size-4" /></Button>
    </Tooltip>
  ),
}
