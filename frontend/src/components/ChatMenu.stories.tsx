import type { Meta, StoryObj } from '@storybook/react-vite'
import { within, userEvent, expect } from 'storybook/test'
import { ChatMenu } from './ChatMenu'

const meta: Meta<typeof ChatMenu> = {
  title: 'Chat/ChatMenu',
  component: ChatMenu,
}
export default meta

type Story = StoryObj<typeof ChatMenu>

export const Default: Story = {
  args: { chatId: 'chat-1' },
}

// #1136: on compact width the header hides its inline token/model summary -
// this menu is where it moves to instead, always available regardless of width.
export const WithUsageOnCompact: Story = {
  args: { chatId: 'chat-1', usage: { models: ['gpt-5'], usage: { total_tokens: 5522, input_tokens: 4000, output_tokens: 1522 } } },
  render: args => (
    <div className="w-[360px]">
      <ChatMenu {...args} />
    </div>
  ),
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Chat actions' }))
    expect(canvas.getByText('gpt-5')).toBeInTheDocument()
    expect(canvas.getByText('5,522 tok')).toBeInTheDocument()
  },
}
