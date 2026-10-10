import type { Meta, StoryObj } from '@storybook/react-vite'
import { within, userEvent } from 'storybook/test'
import { ChatList } from './ChatList'
import type { ChatSummary } from '../api'

const now = '2026-06-18T12:00:00Z'
function chat(id: string, title: string, status: ChatSummary['status'] = 'idle', archived?: boolean): ChatSummary {
  return { id, title, system_prompt: '', created_at: now, updated_at: now, status, archived }
}

function githubChat(
  id: string,
  title: string,
  repo: string,
  kind: 'issues' | 'pull' = 'issues',
  status: ChatSummary['status'] = 'idle',
): ChatSummary {
  return {
    id,
    title,
    system_prompt: '',
    created_at: now,
    updated_at: now,
    status,
    github_repo: repo,
    github_url: `https://github.com/${repo}/${kind}/${id.match(/\d+$/)?.[0] ?? 1}`,
  }
}

function originChat(id: string, title: string, badge?: string): ChatSummary {
  return {
    id, title, system_prompt: '', created_at: now, updated_at: now, status: 'idle',
    origin: { extension: 'remarkable', label: title, badge },
  }
}

const CHATS: ChatSummary[] = [
  chat('1', 'Best time to visit Dublin'),
  chat('2', 'Local LLM models for my hardware'),
  chat('3', 'Debounce vs throttle in React'),
  chat('4', 'Postgres connection pooling'),
]

// Direct chats interleaved with GitHub-originated ones, varied status, issue and PR refs across two repos.
const MIXED_CHATS: ChatSummary[] = [
  chat('direct-1', 'Best time to visit Dublin'),
  githubChat('github-quack-386', 'Chats aren’t filterable by origin', 'fagerbergj/quack', 'issues', 'running'),
  chat('direct-2', 'Debounce vs throttle in React', 'failed'),
  githubChat('github-quack-350', 'Force-push the work branch', 'fagerbergj/quack', 'pull', 'idle'),
  chat('direct-3', 'Postgres connection pooling'),
  githubChat('github-games-3', 'Flappy bird collision tuning', 'fagerbergj/games', 'issues', 'needs_input'),
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
    onArchive: (id: string) => alert(`archive ${id}`),
    onUnarchive: (id: string) => alert(`unarchive ${id}`),
    onExpandArchived: () => {},
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

// GitHub rows carry a repo badge and an Issue/PR badge, both linking out; filtering lives in the funnel popover.
export const MixedOrigin: Story = {
  args: { chats: MIXED_CHATS, activeChatId: null },
}

// Non-idle rows (running/failed/needs_input) show a small colored dot right
// before the title (blue/red/amber); idle rows stay quiet - no dot at all.
export const StatusDots: Story = {
  args: {
    chats: [
      chat('running-1', 'Currently streaming', 'running'),
      chat('failed-1', 'Hit an error', 'failed'),
      chat('waiting-1', 'Paused on a question', 'needs_input'),
      chat('idle-1', 'Nothing going on', 'idle'),
    ],
    activeChatId: null,
  },
}

// Only open/merged/closed get GitHub's colours; any other badge (e.g. "draft") stays neutral gray.
export const OriginBadgeStates: Story = {
  args: {
    chats: [
      originChat('origin-open', 'Open pull request', 'open'),
      originChat('origin-merged', 'Merged pull request', 'merged'),
      originChat('origin-closed', 'Closed pull request', 'closed'),
      originChat('origin-other', 'Extension-defined badge', 'draft'),
    ],
    activeChatId: null,
  },
}

// A server `next_page_token` surfaces as a "Load more" row at the bottom.
export const WithLoadMore: Story = {
  args: { chats: CHATS, activeChatId: '2', hasMoreChats: true, onLoadMoreChats: () => alert('load more') },
}

// Opens the filter popover and selects the GitHub origin facet - only
// GitHub-originated rows remain, and the funnel shows an active-filter badge.
export const FilteredToGithub: Story = {
  args: { chats: MIXED_CHATS, activeChatId: null },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Filter chats' }))
    await userEvent.click(canvas.getByRole('checkbox', { name: /^GitHub/ }))
  },
}

// Selects the Repo facet for a single repo - narrows across both its issue
// and PR rows, leaving the other repo's row out.
export const FilteredToRepo: Story = {
  args: { chats: MIXED_CHATS, activeChatId: null },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Filter chats' }))
    await userEvent.click(canvas.getByRole('checkbox', { name: /^fagerbergj\/quack/ }))
  },
}

// Archived rows get one always-visible kebab (Restore, Delete), absolutely positioned so row height
// matches an active row's.
export const WithArchivedChats: Story = {
  args: {
    chats: [chat('active-1', 'Current project notes')],
    // In the app archivedChats stays undefined until the section is first expanded.
    archivedChats: [
      chat('archived-1', 'Old debugging session', 'idle', true),
      chat('archived-2', 'Abandoned experiment', 'idle', true),
    ],
    activeChatId: null,
  },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: /^Archived/ }))
  },
}

// Opens an archived row's kebab menu to reveal Restore and Delete. Checked at
// mobile width too - the off-canvas drawer is how this list is reached there.
export const ArchivedRowKebabMenu: Story = {
  args: {
    chats: [],
    archivedChats: [chat('archived-1', 'Old debugging session', 'idle', true)],
    activeChatId: null,
  },
  parameters: { renderCheck: { viewports: ['mobile', 'desktop'] } },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: /^Archived/ }))
    await userEvent.click(canvas.getByRole('button', { name: 'Chat actions' }))
  },
}

// An active row's kebab holds the sole Archive action; there is no bare row button.
export const ActiveRowKebabMenu: Story = {
  args: {
    chats: [chat('active-1', 'Current project notes')],
    activeChatId: null,
  },
  parameters: { renderCheck: { viewports: ['mobile', 'desktop'] } },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Chat actions' }))
  },
}
