import type { Meta, StoryObj } from '@storybook/react-vite'
import { within, userEvent } from 'storybook/test'
import { MemoryEntry } from './MemoryEntry'
import type { Memory } from '../api'

const meta: Meta<typeof MemoryEntry> = {
  title: 'Memory/MemoryEntry',
  component: MemoryEntry,
  args: { onForget: async () => {} },
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
}

export const Default: Story = {
  args: { memory: REPO_FACT },
}

// A search hit (?q=...) carries a score - List entries never do.
export const SearchResult: Story = {
  args: { memory: { ...REPO_FACT, id: 's1', score: 0.87 } },
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

// Clicking Forget reveals the Confirm/Cancel step (no delete has happened yet).
export const ConfirmingForget: Story = {
  args: { memory: REPO_FACT },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: /^Forget:/ }))
  },
}

// onForget rejects - the row surfaces the error inline rather than silently reverting.
export const ForgetFailed: Story = {
  args: {
    memory: REPO_FACT,
    onForget: async () => {
      throw new Error('Forget failed (500)')
    },
  },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: /^Forget:/ }))
    await userEvent.click(canvas.getByRole('button', { name: 'Confirm' }))
  },
}
