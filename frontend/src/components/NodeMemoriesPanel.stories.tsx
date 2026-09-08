import type { Meta, StoryObj } from '@storybook/react-vite'
import { within, waitFor } from 'storybook/test'
import { NodeMemoriesPanel } from './NodeMemoriesPanel'

const meta: Meta<typeof NodeMemoriesPanel> = {
  title: 'Chat/NodeMemoriesPanel',
  component: NodeMemoriesPanel,
  parameters: { layout: 'fullscreen' },
}
export default meta

type Story = StoryObj<typeof NodeMemoriesPanel>

// The panel talks to the real REST client (frontend/src/api.ts) - a story
// stubs window.fetch with canned GET .../memories responses per chat id,
// same pattern ArtifactPanel.stories.tsx uses (no MSW in this repo).
function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
}

const FIXTURES: Record<string, unknown[]> = {
  'chat-empty': [],
  'chat-mix': [
    { id: 'm1', source: 'prefill', content: 'the deploy script needs sudo', tier: 'unverified', score: 0.82 },
    { id: 'm2', source: 'tool', content: 'retry uploads on 5xx', tier: 'verified', score: 0.91 },
  ],
  'chat-votes': [
    { id: 'm1', source: 'prefill', content: 'the deploy script needs sudo', tier: 'verified', vote: 'supported', reason: 'matched the diff' },
    { id: 'm2', source: 'prefill', content: 'unrelated note', tier: 'unverified', vote: 'not_relevant' },
    { id: 'm3', source: 'tool', content: 'wrong assumption', tier: 'unverified', vote: 'contradicted', reason: 'diff shows the opposite' },
  ],
  'chat-own-vote': [
    { id: 'm1', source: 'prefill', content: 'the deploy script needs sudo', tier: 'verified', vote: 'supported', own_vote: 'up' },
  ],
}

window.fetch = async (input: RequestInfo | URL) => {
  const url = typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url
  for (const [chatId, memories] of Object.entries(FIXTURES)) {
    if (url.includes(`/chats/${chatId}/`)) return jsonResponse({ memories })
  }
  return jsonResponse({ memories: [] })
}

export const Empty: Story = {
  args: { chatId: 'chat-empty', nodeId: 'node-1', onClose: () => {} },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await waitFor(() => canvas.getByText('This node received no memories'))
  },
}

export const UnverifiedAndVerifiedMix: Story = {
  args: { chatId: 'chat-mix', nodeId: 'node-1', onClose: () => {} },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await waitFor(() => canvas.getByText('the deploy script needs sudo'))
  },
}

export const WithJudgeVotes: Story = {
  args: { chatId: 'chat-votes', nodeId: 'node-1', onClose: () => {} },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await waitFor(() => canvas.getByText('supported'))
  },
}

export const OwnVoteActive: Story = {
  args: { chatId: 'chat-own-vote', nodeId: 'node-1', onClose: () => {} },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await waitFor(() => canvas.getByRole('button', { name: 'Upvote' }))
  },
}
