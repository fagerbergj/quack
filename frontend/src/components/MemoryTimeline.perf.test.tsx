// @vitest-environment jsdom
//
// #1286: groupByAge and MemoryEntry's date formatting were rebuilt on every
// render (e.g. a sibling row's vote), scaling linearly with row count.
import { describe, it, expect, beforeAll } from 'vitest'
import { act, createElement, useState, Profiler } from 'react'
import { createRoot } from 'react-dom/client'
import { MemoryTimeline } from './MemoryTimeline'
import type { Memory } from '../api'

beforeAll(() => {
  // @ts-expect-error react act env flag
  globalThis.IS_REACT_ACT_ENVIRONMENT = true
})

function makeRows(n: number): Memory[] {
  const now = Date.now()
  return Array.from({ length: n }, (_, i) => ({
    id: `m${i}`,
    content: `Realistic memory content number ${i} - a sentence describing something the agent learned during a prior run, long enough to approximate real row text.`,
    bucket: i % 5 === 0 ? 'global' : `repo:project-${i % 7}`,
    author: i % 3 === 0 ? 'code-implementer' : 'code-reviewer',
    // Spread across the age bands so groupByAge does real grouping work.
    timestamp: new Date(now - (i * 3600_000 * 7)).toISOString(),
    kind: 'repo',
    upvotes: i % 4,
    downvotes: i % 2,
    last_upvoted_at: i % 2 === 0 ? new Date(now - i * 60_000).toISOString() : undefined,
    last_recalled_at: i % 3 === 0 ? new Date(now - i * 120_000).toISOString() : undefined,
  })) as unknown as Memory[]
}

// Uses React's Profiler `actualDuration` (render-phase cost only) rather
// than a wall-clock wrap around mount, which would also count jsdom's
// commit/layout overhead unrelated to the component's own render work.
async function timeRender(n: number): Promise<number> {
  const rows = makeRows(n)
  const host = document.createElement('div')
  document.body.appendChild(host)
  const root = createRoot(host)
  let actualDuration = 0
  const onRender = (_id: string, _phase: string, duration: number) => { actualDuration += duration }
  await act(async () => {
    root.render(
      createElement(
        Profiler,
        { id: 'timeline', onRender },
        createElement(MemoryTimeline, {
          memories: rows,
          onForget: async () => {},
          onVote: async () => {},
          grouped: true,
        }),
      ),
    )
  })
  act(() => root.unmount())
  host.remove()
  return actualDuration
}

describe('MemoryTimeline render cost', () => {
  it('renders 20 rows vs 200 rows', { timeout: 30000 }, async () => {
    // Warm up JIT once, untimed.
    await timeRender(20)

    const N = 10
    let total20 = 0, total200 = 0
    for (let i = 0; i < N; i++) total20 += await timeRender(20)
    for (let i = 0; i < N; i++) total200 += await timeRender(200)

    const avg20 = total20 / N
    const avg200 = total200 / N
    const ratio = avg200 / avg20
    console.log(`20 rows: avg actualDuration=${avg20.toFixed(3)}ms  200 rows: avg=${avg200.toFixed(3)}ms  ratio=${ratio.toFixed(2)}x`)
    // 200 rows must cost more than 20 (real per-row work) but not
    // super-linearly - linear caps the ratio near 10x, so a loose upper bound
    // catches a pathological (e.g. quadratic) regression without pinning an
    // absolute ms figure, which would be flaky under jsdom/CI load.
    expect(avg200).toBeGreaterThan(avg20)
    expect(ratio).toBeLessThan(30)
  })

  it('an unrelated parent re-render does not re-run groupByAge or re-render untouched rows', { timeout: 30000 }, async () => {
    const rows = makeRows(20)
    // Module-scope, stable identity - an inline arrow here would be a fresh
    // prop every Harness render and defeat memo(MemoryEntry) just like the
    // bug this test guards against (mirrors MemoryTab's real useCallback fix).
    const onForget = async () => {}
    const onVote = async () => {}
    const host = document.createElement('div')
    document.body.appendChild(host)
    const root = createRoot(host)
    let renderCount = 0
    let bump: (() => void) | undefined

    function Harness() {
      const [, setN] = useState(0)
      bump = () => setN(v => v + 1)
      renderCount++
      return createElement(MemoryTimeline, {
        memories: rows, // same reference every render
        onForget,
        onVote,
        grouped: true,
      })
    }

    await act(async () => { root.render(createElement(Harness)) })
    expect(renderCount).toBe(1)

    // Re-render the parent (as an unrelated state change would) with the
    // exact same `memories` array - groupByAge's useMemo should bail, and
    // memo(MemoryEntry) should skip every row since `memory` refs are stable.
    // Compared against this run's own mount cost (not an absolute ms bound,
    // which is flaky under jsdom/CI load - audit note) so the assertion
    // holds regardless of machine speed.
    let mountDuration = 0
    let updateDuration = 0
    const onRender = (_id: string, phase: string, d: number) => {
      if (phase === 'mount') mountDuration += d
      else updateDuration += d
    }
    const host2 = document.createElement('div')
    document.body.appendChild(host2)
    const root2 = createRoot(host2)
    await act(async () => {
      root2.render(createElement(Profiler, { id: 't2', onRender }, createElement(Harness)))
    })
    await act(async () => { bump?.() })

    console.log(`mount=${mountDuration.toFixed(3)}ms update(unrelated re-render)=${updateDuration.toFixed(3)}ms for 20 rows`)
    // A bailed-out re-render (memo(MemoryEntry) + memoized groupByAge) should
    // be a small fraction of a full mount's render cost, not comparable to it.
    expect(updateDuration).toBeLessThan(mountDuration / 2)

    act(() => { root.unmount(); root2.unmount() })
    host.remove()
    host2.remove()
  })
})
