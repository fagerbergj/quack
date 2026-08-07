import type { Meta, StoryObj } from '@storybook/react-vite'
import { MemoryTimeline } from './MemoryTimeline'
import type { Memory } from '../api'

const meta: Meta<typeof MemoryTimeline> = {
  title: 'Memory/MemoryTimeline',
  component: MemoryTimeline,
  parameters: { layout: 'padded' },
  args: { onForget: async () => {}, now: new Date('2026-08-06T12:00:00Z').getTime() },
}
export default meta

type Story = StoryObj<typeof MemoryTimeline>

function mem(overrides: Partial<Memory> & Pick<Memory, 'id' | 'content' | 'timestamp'>): Memory {
  return { bucket: 'repo:NightsOut', author: 'code-implementer', kind: 'repo', ...overrides }
}

// #746 item 14: a vertical rail carries each entry's date on the left,
// entries grouped into age bands (Today / This week / This month / Older).
export const Populated: Story = {
  args: {
    memories: [
      mem({ id: '1', content: "The user prefers TypeScript over JavaScript for new frontend code.", bucket: 'user:jason', author: 'orchestrator', kind: 'preference', timestamp: '2026-08-06T10:00:00Z' }),
      mem({ id: '2', content: "NightsOut's instrumentation tests need minSdk 30 for DEX version 040.", timestamp: '2026-08-04T18:22:11Z' }),
      mem({ id: '3', content: "A source's own docs beat a blog post about the same API.", bucket: 'role:research', author: 'web-researcher', kind: 'convention', timestamp: '2026-08-02T14:30:00Z' }),
      mem({ id: '4', content: 'The trust gate implementation lives in internal/vetting/, split across node.go, judge.go, checks.go.', bucket: 'repo:quack', author: 'gate-explorer', kind: 'repo', timestamp: '2026-07-20T09:00:00Z' }),
      mem({ id: '5', content: 'Config rejection tests use a writeTemp helper and a baseConfig constant.', bucket: 'repo:quack', author: 'explore-config', kind: 'repo', timestamp: '2026-05-01T09:00:00Z' }),
    ],
  },
}

export const Empty: Story = {
  args: { memories: [] },
}

// Every entry here is well outside "This month" - the whole timeline is one
// "Older" group, so old memories stay visibly distinct even when that's ALL
// there is (no Today/This week bands crowding them).
export const Aged: Story = {
  args: {
    memories: [
      mem({ id: '1', content: 'A stale fact from a much earlier run.', timestamp: '2026-02-10T09:00:00Z' }),
      mem({ id: '2', content: 'An even older fact, kept for context.', bucket: 'role:coding', author: 'code-implementer', kind: 'convention', timestamp: '2025-11-01T09:00:00Z' }),
    ],
  },
}
