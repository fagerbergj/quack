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

// A mixed list: direct chats interleaved with GitHub-originated ones (issue #386),
// with varied status and both issue/PR refs across two repos (issue #396 follow-up).
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

// A mixed list: GitHub-originated rows carry a repo badge and an Issue/PR
// badge - both link out to GitHub - plus (when not idle) a colored status dot
// next to the title. Filtering by Origin/Status/Repo/Type lives entirely in
// the funnel popover.
export const MixedOrigin: Story = {
  args: { chats: MIXED_CHATS, activeChatId: null },
}

// Non-idle rows (running/failed/needs_input/queued) show a small colored dot
// right before the title (blue/red/amber/gray); idle rows stay quiet - no dot
// at all. queued (#417) is gray and non-pulsing - admitted but still waiting
// on the server's max_active_runs slot, distinct from the pulsing blue
// running dot for the one chat actually executing.
export const StatusDots: Story = {
  args: {
    chats: [
      chat('running-1', 'Currently streaming', 'running'),
      chat('queued-1', 'Waiting behind the run cap', 'queued'),
      chat('failed-1', 'Hit an error', 'failed'),
      chat('waiting-1', 'Paused on a question', 'needs_input'),
      chat('idle-1', 'Nothing going on', 'idle'),
    ],
    activeChatId: null,
  },
}

// #870: the origin badge mirrors GitHub's own state colors for open/merged/
// closed - any other badge value (e.g. "draft", or an extension-specific
// label) keeps the existing neutral gray chip rather than inventing a color.
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

// #736: a `next_page_token` from the server surfaces as a "Load more" row at the
// bottom of the list.
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

// Expands the collapsed Archived section: each row gets a "⋮" overflow trigger
// (Restore lives inside it) top-right, next to the same × as active rows - both
// are hover-only and absolutely positioned, so the row height matches an active
// row's exactly. Hover a row here to reveal them; × swaps its label to "Delete
// chat permanently" since it now hard-deletes instead of archiving.
export const WithArchivedChats: Story = {
  args: {
    chats: [chat('active-1', 'Current project notes')],
    // #809: archivedChats is the section's own already-loaded list here -
    // in the app it stays undefined until the section is first expanded.
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

// Opens an archived row's overflow menu to reveal the Restore action. Hovering
// first isn't needed for the click itself, but mirrors how a user reaches it.
export const ArchivedRowOverflowMenu: Story = {
  args: {
    chats: [],
    archivedChats: [chat('archived-1', 'Old debugging session', 'idle', true)],
    activeChatId: null,
  },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: /^Archived/ }))
    await userEvent.click(canvas.getByRole('button', { name: 'Row actions' }))
  },
}
