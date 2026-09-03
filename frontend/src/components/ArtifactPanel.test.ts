import { describe, it, expect } from 'vitest'
import { anchorNotes } from './ArtifactPanel'

const lines = [
  'function review(x) {',
  '  // x may be nil here',
  '  return x.value',
  '}',
]

describe('anchorNotes', () => {
  it('anchors to the line containing the quoted snippet', () => {
    const { byLine, unanchored } = anchorNotes(lines, [
      { ref: { artifact_id: 'a', revision: 1, snippet: 'x may be nil here' }, text: 'needs a null check' },
    ])
    expect(unanchored).toHaveLength(0)
    expect(byLine.get(1)?.[0].text).toBe('needs a null check')
  })

  it('falls back to line_hint when the snippet is not found (stale after an edit)', () => {
    const { byLine, unanchored } = anchorNotes(lines, [
      { ref: { artifact_id: 'a', revision: 1, snippet: 'no longer present', line_hint: 3 }, text: 'fallback note' },
    ])
    expect(unanchored).toHaveLength(0)
    // line_hint is 1-based; line 3 -> index 2.
    expect(byLine.get(2)?.[0].text).toBe('fallback note')
  })

  it('marks a note unanchored when neither the snippet nor line_hint resolves', () => {
    const { byLine, unanchored } = anchorNotes(lines, [
      { ref: { artifact_id: 'a', revision: 1, snippet: 'nowhere', line_hint: 999 }, text: 'lost note' },
    ])
    expect(byLine.size).toBe(0)
    expect(unanchored).toEqual([{ ref: { artifact_id: 'a', revision: 1, snippet: 'nowhere', line_hint: 999 }, text: 'lost note' }])
  })

  it('marks a note with no ref at all unanchored', () => {
    const { unanchored } = anchorNotes(lines, [
      { ref: { artifact_id: 'a', revision: 1 }, text: 'no snippet, no hint' },
    ])
    expect(unanchored).toHaveLength(1)
  })

  it('anchors a multi-line snippet by its first line (#1113 review)', () => {
    const { byLine, unanchored } = anchorNotes(lines, [
      { ref: { artifact_id: 'a', revision: 1, snippet: 'x may be nil here\nreturn x.value' }, text: 'quotes two lines' },
    ])
    expect(unanchored).toHaveLength(0)
    expect(byLine.get(1)?.[0].text).toBe('quotes two lines')
  })

  it('groups multiple notes landing on the same line', () => {
    const { byLine } = anchorNotes(lines, [
      { ref: { artifact_id: 'a', revision: 1, snippet: 'x may be nil' }, text: 'note one' },
      { ref: { artifact_id: 'a', revision: 1, snippet: 'nil here' }, text: 'note two' },
    ])
    expect(byLine.get(1)?.map(n => n.text)).toEqual(['note one', 'note two'])
  })
})
