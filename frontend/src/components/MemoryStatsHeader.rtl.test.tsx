// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { MemoryTab } from './MemoryTab'
import { api } from '../api'
import type { MemoryWeekStats } from '../api'

afterEach(cleanup)

// #1267: the header numbers come from GET /memories/stats via api.getMemoryStats
// - stub that call (not initialStats) so this exercises the real fetch wiring.
const WEEKS: MemoryWeekStats[] = [
  { week: '2026-W35', recalls: 38, supported: 18, contradicted: 8, not_relevant: 4, precision: 0.69, support_share: 0.47, minted: 3, invalidated: 4 },
  { week: '2026-W36', recalls: 61, supported: 40, contradicted: 3, not_relevant: 3, precision: 0.87, support_share: 0.87, minted: 9, invalidated: 0 },
]

describe('MemoryStatsHeader via MemoryTab (#1267)', () => {
  it('renders this-week precision and support share from a stubbed stats response', async () => {
    vi.spyOn(api, 'getMemoryStats').mockResolvedValue({
      weeks: WEEKS,
      scopes: [{ scope: 'repo:quack', live: 42, invalidated: 5 }],
    })
    render(<MemoryTab initialState={{ memories: [], total: 0 }} />)
    expect((await screen.findAllByText('87%')).length).toBeGreaterThan(0)
 expect(screen.getByText('69%')).toBeDefined()// last-4-weeks number for W35
  })

  it('shows a dash, not NaN or 0%, when no votes were cast this week', async () => {
    vi.spyOn(api, 'getMemoryStats').mockResolvedValue({
      weeks: [{ week: '2026-W36', recalls: 0, supported: 0, contradicted: 0, not_relevant: 0, precision: 0, support_share: 0, minted: 0, invalidated: 0 }],
      scopes: [],
    })
    render(<MemoryTab initialState={{ memories: [], total: 0 }} />)
    const dashes = await screen.findAllByText('—')
    expect(dashes.length).toBeGreaterThanOrEqual(2)
    expect(screen.queryByText('NaN%')).toBeNull()
    expect(screen.queryByText('0%')).toBeNull()
  })
})
