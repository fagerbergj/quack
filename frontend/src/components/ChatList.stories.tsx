import type { Meta, StoryObj } from '@storybook/react-vite'
import { within, userEvent } from 'storybook/test'
import { ChatList } from './ChatList'
import type { ChatSummary } from '../api'

const now = '2026-06-18T12:00:00Z'
function chat(id: string, title: string): ChatSummary {
  return { id, title, system_prompt: '', created_at: now, updated_at: now, status: 'idle' }
}

function githubChat(id: string, title: string, repo: string, ref = 'issues/1'): ChatSummary {
  return {
    id,
    title,
    system_prompt: '',
    created_at: now,
    updated_at: now,
    status: 'idle',
    github_repo: repo,
    github_url: `https://github.com/${repo}/${ref}`,
  }
}

const CHATS: ChatSummary[] = [
  chat('1', 'Best time to visit Dublin'),
  chat('2', 'Local LLM models for my hardware'),
  chat('3', 'Debounce vs throttle in React'),
  chat('4', 'Postgres connection pooling'),
]

// A mixed list: direct chats interleaved with GitHub-originated ones (issue #386).
const MIXED_CHATS: ChatSummary[] = [
  chat('direct-1', 'Best time to visit Dublin'),
  githubChat('github-quack-386', 'Chats aren’t filterable by origin', 'fagerbergj/quack', 'issues/386'),
  chat('direct-2', 'Debounce vs throttle in React'),
  githubChat('github-quack-350', 'Force-push the work branch', 'fagerbergj/quack', 'pull/350'),
  chat('direct-3', 'Postgres connection pooling'),
]

const meta: Meta<typeof ChatList> = {
  title: 'Chat/ChatList',
  component: ChatList,
  args: {
    open: true,
    onSelect: (id: string) => alert(`select ${id}`),
    onNewChat: () => alert('new chat'),
    onDelete: (id: string) => alert(`delete ${id}`),
    onCloseMobile: () => {},
  },
  decorators: [Story => <div className="h-[28rem] flex"><Story /></div>],
}
export default meta

type Story = StoryObj<typeof ChatList>

export const WithChats: Story = {
  args: { chats: CHATS, activeChatId: '2' },
}

export const Empty: Story = {
  args: { chats: [], activeChatId: null },
}

// The search box filters the list by title (type "react"/"dublin" to narrow it).
export const Searchable: Story = {
  args: { chats: CHATS, activeChatId: null },
}

// A mixed list: GitHub-originated rows carry the "GH" badge, direct rows don't.
// The All/Direct/GitHub control above the list narrows which rows render —
// click each to see the filter in action.
export const MixedOrigin: Story = {
  args: { chats: MIXED_CHATS, activeChatId: null },
}

// Same mixed list with the "GitHub" filter selected: only badged rows remain.
export const FilteredToGithub: Story = {
  args: { chats: MIXED_CHATS, activeChatId: null },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'GitHub' }))
  },
}

// Same mixed list with the "Direct" filter selected: only unbadged rows remain.
export const FilteredToDirect: Story = {
  args: { chats: MIXED_CHATS, activeChatId: null },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Direct' }))
  },
}
