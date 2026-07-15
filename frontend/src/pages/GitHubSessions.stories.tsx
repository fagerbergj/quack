import type { Meta, StoryObj } from '@storybook/react-vite'
import { GitHubSessionsView } from './GitHubSessions'
import type { ChatSummary } from '../api'

function ghChat(
  n: number,
  repo: string,
  kind: 'issues' | 'pull',
  title: string,
  status: ChatSummary['status'],
  updated: string,
): ChatSummary {
  const [owner, name] = repo.split('/')
  return {
    id: `github-${owner}-${name}-${n}`,
    title,
    system_prompt: '',
    created_at: updated,
    updated_at: updated,
    status,
    github_repo: repo,
    github_url: `https://github.com/${repo}/${kind}/${n}`,
  }
}

const CHATS: ChatSummary[] = [
  ghChat(249, 'fagerbergj/quack', 'issues', 'Capture false-positive corrections in memory', 'running', '2026-07-15T21:51:00Z'),
  ghChat(257, 'fagerbergj/quack', 'pull', 'GitHub tab listing GitHub-originated sessions', 'needs_input', '2026-07-15T21:46:00Z'),
  ghChat(246, 'fagerbergj/quack', 'pull', 'quack:implement label drives implementation', 'idle', '2026-07-15T20:23:00Z'),
  ghChat(3, 'fagerbergj/games', 'issues', 'Flappy bird collision tuning', 'failed', '2026-07-14T09:00:00Z'),
  // a non-GitHub chat that must be filtered out of the tab
  { id: 'local-abc', title: 'Local scratch chat', system_prompt: '', created_at: '2026-07-15T10:00:00Z', updated_at: '2026-07-15T10:00:00Z', status: 'idle' },
]

const meta: Meta<typeof GitHubSessionsView> = {
  title: 'Pages/GitHubSessions',
  component: GitHubSessionsView,
  decorators: [Story => <div className="h-[32rem]"><Story /></div>],
}
export default meta

type Story = StoryObj<typeof GitHubSessionsView>

// Grouped by repo, sorted by recency; the ↗ links out to the issue/PR, the local
// chat is filtered out. Toggle the toolbar theme to check dark mode.
export const Populated: Story = {
  args: { chats: CHATS },
}

export const Empty: Story = {
  args: { chats: [] },
}
