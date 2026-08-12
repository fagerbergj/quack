// @vitest-environment jsdom
import { describe, it, expect } from 'vitest'
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { UsageSummary } from './UsageSummary'
import type { Usage } from '../generated'

// Static-markup assertions (see DagNode.test.ts) - <details>/<summary> is a
// native disclosure, so the expandable breakdown is always present in the
// DOM; no click simulation needed to prove it renders.
function html(models: string[], usage?: Usage): string {
  return renderToStaticMarkup(createElement(UsageSummary, { models, usage }))
}

describe('UsageSummary - a fresh chat with no run yet', () => {
  it('renders nothing', () => {
    expect(html([])).toBe('')
  })
})

describe('UsageSummary - model chip(s)', () => {
  it('shows a single turn model', () => {
    const out = html(['gpt-oss-120b'])
    expect(out).toContain('gpt-oss-120b')
  })

  it('shows multiple distinct DAG node models', () => {
    const out = html(['gpt-oss-120b', 'qwen3-coder-next'])
    expect(out).toContain('gpt-oss-120b')
    expect(out).toContain('qwen3-coder-next')
  })
})

describe('UsageSummary - session token total and breakdown', () => {
  const usage: Usage = { input_tokens: 1000, output_tokens: 200, reasoning_tokens: 50, cached_tokens: 300, total_tokens: 1250 }

  it('shows the total in the collapsed summary', () => {
    expect(html([], usage)).toContain('1,250 tok')
  })

  it('the expandable detail carries the full input/output/reasoning/cached split', () => {
    const out = html([], usage)
    expect(out).toContain('1,000') // input
    expect(out).toContain('200') // output
    expect(out).toContain('50') // reasoning
    expect(out).toContain('300') // cached
  })

  it('shows the cache rate when cached > 0', () => {
    expect(html([], usage)).toContain('30%') // 300/1000
  })

  it('omits the cache rate row when nothing was cached', () => {
    const out = html([], { ...usage, cached_tokens: 0 })
    expect(out).not.toContain('Cache rate')
  })

  // Regression: UsageRow used to hide a row on a falsy value, so a
  // genuinely-zero dimension (e.g. no reasoning tokens this turn) silently
  // vanished instead of reading "0" - indistinguishable from "not tracked".
  it('renders all four breakdown rows even when their values are 0', () => {
    const out = html([], { input_tokens: 0, output_tokens: 0, reasoning_tokens: 0, cached_tokens: 0, total_tokens: 100 })
    expect(out).toContain('Input')
    expect(out).toContain('Output')
    expect(out).toContain('Reasoning')
    expect(out).toContain('Cached')
    expect(out.match(/>0</g)?.length).toBe(4)
  })
})
