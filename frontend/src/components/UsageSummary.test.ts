// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { createElement } from 'react'
import { cleanup, render } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { UsageSummary, fmtCost } from './UsageSummary'
import type { Usage } from '../generated'

afterEach(cleanup)

// The breakdown lives in a popover, so open it (when there is a total) before reading the page.
async function html(models: string[], usage?: Usage): Promise<string> {
  render(createElement(UsageSummary, { models, usage }))
  const trigger = document.body.querySelector('button')
  if (trigger?.textContent?.includes('tokens')) await userEvent.click(trigger)
  return document.body.innerHTML
}

describe('UsageSummary - a fresh chat with no run yet', () => {
  it('renders nothing', async () => {
    expect(await html([])).toBe('<div></div>') // the RTL container, nothing of ours
  })
})

describe('UsageSummary - model chip(s)', () => {
  it('shows a single turn model', async () => {
    const out = await html(['gpt-oss-120b'])
    expect(out).toContain('gpt-oss-120b')
  })

  it('shows multiple distinct DAG node models', async () => {
    const out = await html(['gpt-oss-120b', 'qwen3-coder-next'])
    expect(out).toContain('gpt-oss-120b')
    expect(out).toContain('qwen3-coder-next')
  })
})

describe('UsageSummary - session token total and breakdown', () => {
  const usage: Usage = { input_tokens: 1000, output_tokens: 200, reasoning_tokens: 50, cached_tokens: 300, total_tokens: 1250 }

  it('shows the total in the collapsed summary', async () => {
    expect(await html([], usage)).toContain('1,250 tokens')
  })

  it('the expandable detail carries the full input/output/reasoning/cached split', async () => {
    const out = await html([], usage)
    expect(out).toContain('1,000') // input
    expect(out).toContain('200') // output
    expect(out).toContain('50') // reasoning
    expect(out).toContain('300') // cached
  })

  it('shows the cache rate when cached > 0', async () => {
    expect(await html([], usage)).toContain('30%') // 300/1000
  })

  it('omits the cache rate row when nothing was cached', async () => {
    const out = await html([], { ...usage, cached_tokens: 0 })
    expect(out).not.toContain('Cache rate')
  })

  // A genuinely-zero dimension must read "0"; hiding it would be indistinguishable from "not tracked".
  it('renders all four breakdown rows even when their values are 0', async () => {
    const out = await html([], { input_tokens: 0, output_tokens: 0, reasoning_tokens: 0, cached_tokens: 0, total_tokens: 100 })
    expect(out).toContain('Input')
    expect(out).toContain('Output')
    expect(out).toContain('Reasoning')
    expect(out).toContain('Cached')
    expect(out.match(/>0</g)?.length).toBe(4)
  })
})

describe('UsageSummary - cost', () => {
  it('shows the chat cost beside the token total and in the breakdown', async () => {
    const out = await html(['m'], { input_tokens: 10, output_tokens: 5, reasoning_tokens: 0, cached_tokens: 0, total_tokens: 15, cost_usd: 0.4234 })
    expect(out).toContain('15 tokens · $0.42')
    expect(out).toContain('Cost')
  })
  it('omits cost when no call was priced', async () => {
    const out = await html(['m'], { input_tokens: 10, output_tokens: 5, reasoning_tokens: 0, cached_tokens: 0, total_tokens: 15 })
    expect(out).not.toContain('$')
  })
  it('never rounds a priced chat down to $0.00', async () => {
    expect(fmtCost(0.004)).toBe('<$0.01')
    expect(fmtCost(0)).toBe('$0.00')
    expect(fmtCost(12.346)).toBe('$12.35')
  })
})
