import type { Meta, StoryObj } from '@storybook/react-vite'
import { A2uiSurfaceBox } from './A2uiArtifact'
import { ChatStoreProvider } from '../state/ChatStoreProvider'
import { pr9 } from './A2uiSurface.fixtures'

const meta: Meta<typeof A2uiSurfaceBox> = {
  title: 'Chat/A2uiArtifact',
  component: A2uiSurfaceBox,
  parameters: { layout: 'padded' },
  decorators: [Story => <ChatStoreProvider><Story /></ChatStoreProvider>],
}
export default meta

type Story = StoryObj<typeof A2uiSurfaceBox>

// The lazily loaded renderer as the transcript and artifact panel mount it.
export const InChat: Story = { args: { chatId: 'chat-1', content: pr9.first } }
