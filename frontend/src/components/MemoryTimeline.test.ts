import { describe, it, expect } from 'vitest'
import { groupByAge } from './MemoryTimeline'
import type { Memory } from '../api'

const NOW = new Date('2026-08-06T12:00:00Z').getTime()

function mem(id: string, timestamp: string): Memory {
  return { id, content: `content ${id}`, bucket: 'repo:x', author: 'a', kind: 'repo', timestamp }
}

describe('groupByAge (#746 item 14 - grouped by age only)', () => {
  it('buckets Today / This week / This month / Older correctly', () => {
    const memories = [
      mem('today', '2026-08-06T10:00:00Z'),        // 2h ago
      mem('this-week', '2026-08-02T12:00:00Z'),     // 4d ago
      mem('this-month', '2026-07-20T12:00:00Z'),    // ~17d ago
      mem('older', '2026-05-01T12:00:00Z'),         // months ago
    ]
    const groups = groupByAge(memories, NOW)
    expect(groups.map(g => g.label)).toEqual(['Today', 'This week', 'This month', 'Older'])
    expect(groups.map(g => g.memories.map(m => m.id))).toEqual([['today'], ['this-week'], ['this-month'], ['older']])
  })

  it('groups by age only, never by retrieval usage (#747 is a separate PR)', () => {
    // Two memories from the same age band stay in ONE group regardless of
    // any other property - there is no retrieval-usage data to sort by yet.
    const memories = [mem('a', '2026-08-06T09:00:00Z'), mem('b', '2026-08-06T08:00:00Z')]
    const groups = groupByAge(memories, NOW)
    expect(groups).toHaveLength(1)
    expect(groups[0].label).toBe('Today')
    expect(groups[0].memories).toHaveLength(2)
  })

  it('keeps non-contiguous runs of the same band as separate groups, preserving input order', () => {
    const memories = [
      mem('a', '2026-08-06T09:00:00Z'), // Today
      mem('b', '2026-05-01T12:00:00Z'), // Older
      mem('c', '2026-08-06T08:00:00Z'), // Today again, but not adjacent
    ]
    const groups = groupByAge(memories, NOW)
    expect(groups.map(g => g.label)).toEqual(['Today', 'Older', 'Today'])
  })

  it('returns no groups for an empty list', () => {
    expect(groupByAge([], NOW)).toEqual([])
  })
})
