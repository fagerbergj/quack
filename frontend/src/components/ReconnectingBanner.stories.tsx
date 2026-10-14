import type { Meta, StoryObj } from '@storybook/react-vite'
import { ReconnectingBanner } from './ReconnectingBanner'

const meta: Meta<typeof ReconnectingBanner> = {
  title: 'Chat/ReconnectingBanner',
  component: ReconnectingBanner,
  parameters: { layout: 'padded', renderCheck: { viewports: ['mobile', 'desktop'] } },
}
export default meta
type Story = StoryObj<typeof ReconnectingBanner>

export const Reconnecting: Story = { args: { reconnecting: true } }
