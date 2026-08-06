import type { Meta, StoryObj } from '@storybook/react-vite'
import { MemoryTab } from './MemoryTab'
import type { Memory } from '../api'

// initialState (a story/test-only seam - see MemoryTab.tsx) pre-seeds the tab
// and skips its live fetch, so every state below renders with no backend.
const meta: Meta<typeof MemoryTab> = {
  title: 'Memory/MemoryTab',
  component: MemoryTab,
  parameters: { layout: 'fullscreen' },
  decorators: [Story => <div className="h-[37.5rem] bg-gray-50 dark:bg-gray-900"><Story /></div>],
}
export default meta

type Story = StoryObj<typeof MemoryTab>

const MEMORIES: Memory[] = [
  {
    id: 'e5f4a1',
    content: "NightsOut's instrumentation tests need minSdk 30 for DEX version 040.",
    bucket: 'repo:NightsOut',
    author: 'code-implementer',
    timestamp: '2026-08-04T18:22:11Z',
    kind: 'repo',
  },
  {
    id: 'a1b2c3',
    content: 'The user prefers TypeScript over JavaScript for new frontend code.',
    bucket: 'user:jason',
    author: 'orchestrator',
    timestamp: '2026-08-03T09:05:00Z',
    kind: 'preference',
  },
  {
    id: 'f9e8d7',
    content: "A source's own docs beat a blog post about the same API.",
    bucket: 'role:research',
    author: 'web-researcher',
    timestamp: '2026-08-01T14:30:00Z',
    kind: 'convention',
  },
]

export const Populated: Story = {
  args: { initialState: { memories: MEMORIES, total: MEMORIES.length } },
}

export const Empty: Story = {
  args: { initialState: { memories: [], total: 0 } },
}

export const ErrorState: Story = {
  args: { initialState: { memories: [], total: 0, error: 'memory: list "task": qdrant unreachable' } },
}

// A search result set: every entry carries a score, ranked descending -
// distinct from Populated (a plain listing), where score is meaningless.
export const SearchResults: Story = {
  args: {
    initialState: {
      memories: MEMORIES.map((m, i) => ({ ...m, score: 0.91 - i * 0.15 })),
      total: MEMORIES.length,
    },
  },
}
