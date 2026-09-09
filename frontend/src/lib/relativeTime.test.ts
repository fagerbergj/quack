import { describe, it, expect } from 'vitest'
import { relativeTime } from './relativeTime'

describe('relativeTime', () => {
  it('returns null for undefined/malformed input', () => {
    expect(relativeTime(undefined)).toBeNull()
    expect(relativeTime('not a date')).toBeNull()
  })

  it('buckets by magnitude, each just under its unit\'s upper bound', () => {
    const now = Date.now()
    const ago = (ms: number) => new Date(now - ms).toISOString()
    expect(relativeTime(ago(0))).toBe('just now')
    expect(relativeTime(ago(59_000))).toBe('just now')
 expect(relativeTime(ago(5* 60_000))).toBe('5m ago')
 expect(relativeTime(ago(3* 3600_000))).toBe('3h ago')
 expect(relativeTime(ago(2* 86_400_000))).toBe('2d ago')
 expect(relativeTime(ago(60* 86_400_000))).toBe('2mo ago')
 expect(relativeTime(ago(400* 86_400_000))).toBe('1y ago')
  })

  it('never goes negative for a future timestamp', () => {
    expect(relativeTime(new Date(Date.now() + 60_000).toISOString())).toBe('just now')
  })
})
