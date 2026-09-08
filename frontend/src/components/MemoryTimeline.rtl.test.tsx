// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { MemoryTimeline } from './MemoryTimeline'
import type { Memory } from '../api'

afterEach(cleanup)

function mem(id: string, timestamp: string): Memory {
  return { id, content: `content ${id}`, bucket: 'repo:x', author: 'a', kind: 'repo', timestamp }
}

// #1266 review: groupByAge assumes time-ordered input. A non-time sort
// (upvotes/score/downvotes/recalls/last_recalled) hands the timeline a page
// with non-monotonic ages - grouped=false must skip the age-band headers
// entirely rather than render repeating, interleaved "Today...Older...Today".
describe('MemoryTimeline grouping (#1266)', () => {
  const nonTimeOrderedMemories = [
    mem('a', '2026-02-10T09:00:00Z'), // Older
    mem('b', '2026-08-06T10:00:00Z'), // Today
    mem('c', '2026-07-20T12:00:00Z'), // This month
  ]
  const now = new Date('2026-08-06T12:00:00Z').getTime()

  it('grouped=true (default) renders age-band headers', () => {
    render(<MemoryTimeline memories={nonTimeOrderedMemories} onForget={async () => {}} onVote={async () => {}} now={now} />)
    expect(screen.getByText('Older')).toBeTruthy()
    expect(screen.getByText('Today')).toBeTruthy()
    expect(screen.getByText('This month')).toBeTruthy()
  })

  it('grouped=false renders every row with no age-band headers', () => {
    render(<MemoryTimeline memories={nonTimeOrderedMemories} onForget={async () => {}} onVote={async () => {}} now={now} grouped={false} />)
    expect(screen.queryByText('Older')).toBeNull()
    expect(screen.queryByText('Today')).toBeNull()
    expect(screen.queryByText('This month')).toBeNull()
    expect(screen.getByText('content a')).toBeTruthy()
    expect(screen.getByText('content b')).toBeTruthy()
    expect(screen.getByText('content c')).toBeTruthy()
  })
})
