import type { Meta, StoryObj } from '@storybook/react-vite'
import { Card } from './card'

const meta: Meta<typeof Card> = {
  title: 'UI/Card',
  component: Card,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof Card>

// A flat well nests inside a card without a second border.
export const Nested: Story = {
  render: () => (
    <Card className="space-y-3 p-4">
      <h3 className="text-sm font-semibold">Card</h3>
      <p className="text-sm text-muted-foreground">Surfaces are flat with a hairline border; only floating layers carry a shadow.</p>
      <Card variant="flat" className="p-3 text-sm">Flat well</Card>
    </Card>
  ),
}
