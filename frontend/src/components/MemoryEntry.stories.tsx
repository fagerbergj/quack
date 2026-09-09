import type { Meta, StoryObj } from '@storybook/react-vite'
import { within, userEvent } from 'storybook/test'
import { MemoryEntry } from './MemoryEntry'
import type { Memory } from '../api'

const meta: Meta<typeof MemoryEntry> = {
  title: 'Memory/MemoryEntry',
  component: MemoryEntry,
  args: { onForget: async () => {}, onVote: async () => {} },
}
export default meta

type Story = StoryObj<typeof MemoryEntry>

const REPO_FACT: Memory = {
  id: 'e5f4a1',
  content: "NightsOut's instrumentation tests need minSdk 30 for DEX version 040.",
  bucket: 'repo:NightsOut',
  author: 'code-implementer',
  timestamp: '2026-08-04T18:22:11Z',
  kind: 'repo',
  vote_score: 0,
  upvotes: 0,
  downvotes: 0,
  tier: 'unverified',
}

export const Default: Story = {
  args: { memory: REPO_FACT },
}

// A search hit (?q=...) carries a score - List entries never do.
export const SearchResult: Story = {
  args: { memory: { ...REPO_FACT, id: 's1', score: 0.87 } },
}

// The three lifecycle tiers (design doc §3/§8 step 6). A missing status
// (Default, above) reads as unverified - a memory minted before the field
// existed - so it isn't restated as its own story.
export const Reinforced: Story = {
  args: { memory: { ...REPO_FACT, id: 'r1', status: 'reinforced', reinforcement_count: 3 } },
}

export const Invalidated: Story = {
  args: {
    memory: {
      ...REPO_FACT,
      id: 'i1',
      status: 'invalidated',
      invalidation_reason: 'pr closed unmerged',
    },
  },
}

// A status this build doesn't recognize yet (server ahead of a stale
// frontend build) - neutral-gray, never mislabeled as unverified.
export const UnknownStatus: Story = {
  args: {
    memory: { ...REPO_FACT, id: 'u1', status: 'pending_review' as Memory['status'] },
  },
}

// A long fact wraps instead of overflowing the row.
export const LongContent: Story = {
  args: {
    memory: {
      ...REPO_FACT,
      id: 'e5f4a2',
      content:
        "The consolidator drops a candidate if it's a near-duplicate of an existing fact in the same bucket, judged by cosine similarity above 0.92 - this keeps the corpus from accumulating five slightly-reworded copies of the same instruction across repeated runs.",
    },
  },
}

// Verified tier (upvotes >= 1) plus recall/last-upvote metadata (epic #1255 P4).
export const VerifiedWithRecalls: Story = {
  args: {
    memory: {
      ...REPO_FACT,
      id: 'v1',
      tier: 'verified',
      upvotes: 4,
      downvotes: 1,
      vote_score: 3,
      recalls: 12,
 last_upvoted_at: new Date(Date.now() - 3* 3600_000).toISOString(),
 last_recalled_at: new Date(Date.now() - 45* 60_000).toISOString(),
    },
  },
}

// A consolidation merge absorbed other memories into this one (epic #1255
// P5) - a purple "merged ×N" chip, id list on hover.
export const WithAbsorbedLineage: Story = {
  args: {
    memory: { ...REPO_FACT, id: 'm1', tier: 'verified', upvotes: 2, vote_score: 2, absorbed_ids: ['dup-1', 'dup-2'] },
  },
}

// The caller's own upvote is highlighted (epic #1255 P4) - the accent color
// on the up arrow, not a separate badge.
export const OwnVoteActive: Story = {
  args: {
    memory: { ...REPO_FACT, id: 'ov1', tier: 'verified', upvotes: 2, vote_score: 2, own_vote: 'up' },
  },
}

// #1266 regression check: one tier chip (not a duplicate "unverified"), the
// author/node-id pill neutral rather than hash-red, vote control reachable
// below the text, no horizontal overflow at 390px.
export const MobileViewport: Story = {
  args: { memory: { ...REPO_FACT, author: 'review-new-commits', tier: 'verified', upvotes: 2, vote_score: 2 } },
  parameters: { layout: 'fullscreen' },
  decorators: [Story => (
    <div className="w-[390px] mx-auto overflow-hidden border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-900">
      <Story />
    </div>
  )],
}

// Clicking the kebab reveals the Forget action (moved off the row per the
// UI rule: secondary actions in the "…" menu, not a top-level icon button).
export const ConfirmingForget: Story = {
  args: { memory: REPO_FACT },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Memory actions' }))
    await userEvent.click(canvas.getByRole('menuitem', { name: 'Forget' }))
  },
}

// onForget rejects - the menu surfaces the error inline rather than silently reverting.
export const ForgetFailed: Story = {
  args: {
    memory: REPO_FACT,
    onForget: async () => {
      throw new Error('Forget failed (500)')
    },
  },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Memory actions' }))
    await userEvent.click(canvas.getByRole('menuitem', { name: 'Forget' }))
    await userEvent.click(canvas.getByRole('button', { name: 'Confirm' }))
  },
}
