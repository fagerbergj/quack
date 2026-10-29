import type { Meta, StoryObj } from '@storybook/react-vite'
import { Input, Textarea } from './input'

const meta: Meta<typeof Input> = {
  title: 'UI/Input',
  component: Input,
  parameters: { layout: 'padded', renderCheck: { viewports: ['mobile', 'desktop'] } },
}
export default meta

type Story = StoryObj<typeof Input>

export const Fields: Story = {
  render: () => (
    <div className="space-y-3">
      <Input aria-label="Search" placeholder="Search chats..." />
      <Input aria-label="Disabled" defaultValue="Disabled" disabled />
      <Input aria-label="Invalid" defaultValue="not-a-ref" aria-invalid />
      <Textarea aria-label="Notes" placeholder="Textarea" rows={3} />
    </div>
  ),
}
