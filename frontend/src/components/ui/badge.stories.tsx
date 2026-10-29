import type { Meta, StoryObj } from '@storybook/react-vite'
import { Badge, type BadgeTone } from './badge'

const meta: Meta<typeof Badge> = {
  title: 'UI/Badge',
  component: Badge,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof Badge>

const TONES: BadgeTone[] = ['gray', 'green', 'red', 'yellow', 'amber', 'blue', 'orange', 'lime', 'emerald', 'teal', 'cyan', 'indigo', 'purple', 'pink']

export const Tones: Story = {
  render: () => (
    <div className="flex flex-wrap gap-2">
      {TONES.map(t => <Badge key={t} tone={t}>{t}</Badge>)}
    </div>
  ),
}
