// @vitest-environment jsdom
// #1286: groupByAge and every MemoryEntry were rebuilt on any unrelated
// parent re-render (e.g. a sibling row's vote). Asserted with render counts, never durations - timing bounds flake under jsdom/CI load.
import { describe, it, expect, beforeAll } from 'vitest'
import { act, createElement, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { MemoryTimeline, groupByAgeProbe } from './MemoryTimeline'
import { memoryEntryRenderProbe } from './MemoryEntry'
import type { Memory } from '../api'

beforeAll(() => {
  // @ts-expect-error react act env flag
  globalThis.IS_REACT_ACT_ENVIRONMENT = true
})

function makeRows(n: number): Memory[] {
  const now = Date.now()
  return Array.from({ length: n }, (_, i) => ({
    id: `m${i}`,
    content: `Memory ${i}`,
    bucket: i % 5 === 0 ? 'global' : `repo:project-${i % 7}`,
    author: 'code-reviewer',
    timestamp: new Date(now - i * 3600_000 * 7).toISOString(),
    kind: 'repo',
    upvotes: 0,
    downvotes: 0,
  })) as unknown as Memory[]
}

// Module-scope handlers: an inline arrow would be a fresh prop on every
// Harness render and defeat memo(MemoryEntry), the very thing under test.
const onForget = async () => {}
const onVote = async () => {}

describe('MemoryTimeline memoization', () => {
  it('an unrelated parent re-render re-runs neither groupByAge nor any row', async () => {
    const rows = makeRows(20)
    const host = document.createElement('div')
    document.body.appendChild(host)
    const root = createRoot(host)
    let bump: (() => void) | undefined

    function Harness() {
      const [, setN] = useState(0)
      bump = () => setN(v => v + 1)
      return createElement(MemoryTimeline, { memories: rows, onForget, onVote, grouped: true })
    }

    await act(async () => { root.render(createElement(Harness)) })
    expect(groupByAgeProbe.count).toBe(1)
    expect(memoryEntryRenderProbe.count).toBe(20)

    await act(async () => { bump?.() })
    expect(groupByAgeProbe.count).toBe(1)
    expect(memoryEntryRenderProbe.count).toBe(20)

    // A genuinely new list re-groups and re-renders the rows it contains.
    act(() => root.unmount())
    host.remove()
  })
})
