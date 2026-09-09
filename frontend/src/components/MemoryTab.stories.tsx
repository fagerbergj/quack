import type { Meta, StoryObj } from '@storybook/react-vite'
import { within, userEvent, expect } from 'storybook/test'
import { MemoryTab } from './MemoryTab'
import type { Memory, MemoryWeekStats } from '../api'

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

const WEEK_STATS: MemoryWeekStats[] = [
  { week: '2026-W34', recalls: 55, supported: 30, contradicted: 5, not_relevant: 5, precision: 0.75, support_share: 0.75, minted: 8, invalidated: 2 },
  { week: '2026-W35', recalls: 38, supported: 18, contradicted: 8, not_relevant: 4, precision: 0.6, support_share: 0.47, minted: 3, invalidated: 4 },
  { week: '2026-W36', recalls: 61, supported: 40, contradicted: 3, not_relevant: 3, precision: 0.87, support_share: 0.87, minted: 9, invalidated: 0 },
]

export const Populated: Story = {
  args: {
    initialState: { memories: MEMORIES, total: MEMORIES.length },
    initialStats: {
      weeks: WEEK_STATS,
      scopes: [
        { scope: 'repo:quack', live: 42, invalidated: 5 },
        { scope: 'user:jason', live: 8, invalidated: 0 },
      ],
    },
  },
}

export const Empty: Story = {
  args: { initialState: { memories: [], total: 0 }, initialStats: { weeks: [], scopes: [] } },
}

export const ErrorState: Story = {
  args: { initialState: { memories: [], total: 0, error: 'memory: list "task": qdrant unreachable' }, initialStats: { weeks: [], scopes: [] } },
}

// A search result set: every entry carries a score, ranked descending -
// distinct from Populated (a plain listing), where score is meaningless.
export const SearchResults: Story = {
  args: {
    initialState: {
 memories: MEMORIES.map((m, i) => ({ ...m, score: 0.91 - i* 0.15 })),
      total: MEMORIES.length,
    },
    initialStats: { weeks: [], scopes: [] },
  },
}

// All three lifecycle tiers (design doc §3/§8 step 6) in one list: an
// unverified memory (missing status, MEMORIES[0]), a reinforced one, and an
// invalidated one carrying its reason.
const MIXED_TIERS: Memory[] = [
  MEMORIES[0],
  { ...MEMORIES[1], status: 'reinforced', reinforcement_count: 4 },
  { ...MEMORIES[2], status: 'invalidated', invalidation_reason: 'pr closed unmerged' },
]

export const MixedTiers: Story = {
  args: { initialState: { memories: MIXED_TIERS, total: MIXED_TIERS.length }, initialStats: { weeks: [], scopes: [] } },
}

// Clicking "Show invalidated" is a plain checkbox, off by default - this
// story just demonstrates it's reachable and toggleable, since the fetch it
// triggers has no backend here (initialState skips the live GET).
export const ShowInvalidatedToggled: Story = {
  args: { initialState: { memories: MIXED_TIERS, total: MIXED_TIERS.length }, initialStats: { weeks: [], scopes: [] } },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    const toggle = canvas.getByLabelText('Show invalidated') as HTMLInputElement
    expect(toggle.checked).toBe(false)
    await userEvent.click(toggle)
    expect(toggle.checked).toBe(true)
  },
}
