import { describe, it, expect } from 'vitest'
import {
  summarizeArgs,
  previewLine,
  toolFailed,
  str,
  num,
  bool,
  lineDiff,
  parseUnifiedDiff,
} from './toolFormat'

describe('summarizeArgs', () => {
  it('prefers query, then a file path / command', () => {
    expect(summarizeArgs({ query: 'dublin weather' })).toBe('"dublin weather"')
    expect(summarizeArgs({ dir: '/w', path: 'src/app.ts' })).toBe('"src/app.ts"')
    expect(summarizeArgs({ dir: '/w', command: 'go test ./...' })).toBe('"go test ./..."')
  })
  it('returns empty when no representative arg is present', () => {
    expect(summarizeArgs({ depth: 2 })).toBe('')
  })
})

describe('previewLine', () => {
  it('collapses whitespace/newlines to a single line', () => {
    expect(previewLine('line one\n  line two\t\tline three')).toBe('line one line two line three')
  })
  it('truncates with an ellipsis past the cap', () => {
    expect(previewLine('a'.repeat(100), 10)).toBe('a'.repeat(10) + '…')
  })
  it('leaves short text untouched', () => {
    expect(previewLine('short')).toBe('short')
  })
})

describe('toolFailed', () => {
  it('reports failed when the result carries an error field', () => {
    expect(toolFailed({ error: 'boom' })).toBe(true)
  })
  it('reports not-failed for a normal result, or no result yet', () => {
    expect(toolFailed({ exit_code: 0 })).toBe(false)
    expect(toolFailed(undefined)).toBe(false)
    expect(toolFailed('plain string result')).toBe(false)
  })
})

describe('typed arg extractors', () => {
  it('read string/number/boolean fields, else undefined', () => {
    const bag = { path: 'a.ts', replacements: 3, replace_all: true }
    expect(str(bag, 'path')).toBe('a.ts')
    expect(num(bag, 'replacements')).toBe(3)
    expect(bool(bag, 'replace_all')).toBe(true)
    expect(str(bag, 'replacements')).toBeUndefined() // wrong type
    expect(num(bag, 'missing')).toBeUndefined()
    expect(str(null, 'path')).toBeUndefined() // non-object
  })
})

describe('lineDiff (edit_file old → new)', () => {
  it('marks a changed line as remove-then-add and keeps surrounding context', () => {
    const d = lineDiff('a\nb\nc', 'a\nB\nc')
    expect(d).toEqual([
      { type: 'context', text: 'a' },
      { type: 'remove', text: 'b' },
      { type: 'add', text: 'B' },
      { type: 'context', text: 'c' },
    ])
  })

  it('handles a pure insertion (all added, no removes)', () => {
    const d = lineDiff('a\nc', 'a\nb\nc')
    expect(d).toEqual([
      { type: 'context', text: 'a' },
      { type: 'add', text: 'b' },
      { type: 'context', text: 'c' },
    ])
  })

  it('handles a pure deletion', () => {
    const d = lineDiff('a\nb\nc', 'a\nc')
    expect(d).toEqual([
      { type: 'context', text: 'a' },
      { type: 'remove', text: 'b' },
      { type: 'context', text: 'c' },
    ])
  })

  it('returns no lines for two empty strings', () => {
    expect(lineDiff('', '')).toEqual([])
  })

  it('preserves indentation exactly (whitespace-sensitive replacement)', () => {
    const d = lineDiff('  return x', '    return x')
    expect(d).toEqual([
      { type: 'remove', text: '  return x' },
      { type: 'add', text: '    return x' },
    ])
  })
})

describe('parseUnifiedDiff (git_diff)', () => {
  it('classifies +/- content lines, and +++/---/@@ as meta', () => {
    const diff = [
      'diff --git a/x.go b/x.go',
      '--- a/x.go',
      '+++ b/x.go',
      '@@ -1,3 +1,3 @@',
      ' unchanged',
      '-gone',
      '+added',
    ].join('\n')
    expect(parseUnifiedDiff(diff)).toEqual([
      { type: 'meta', text: 'diff --git a/x.go b/x.go' },
      { type: 'meta', text: '--- a/x.go' },
      { type: 'meta', text: '+++ b/x.go' },
      { type: 'meta', text: '@@ -1,3 +1,3 @@' },
      { type: 'context', text: ' unchanged' },
      { type: 'remove', text: '-gone' },
      { type: 'add', text: '+added' },
    ])
  })

  it('returns no lines for an empty diff', () => {
    expect(parseUnifiedDiff('')).toEqual([])
  })
})
