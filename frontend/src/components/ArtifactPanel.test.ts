import { describe, it, expect } from 'vitest'
import {
  anchorNotes,
  selectPrimaryOutput,
  resolveScoredRevision,
  toAscending,
  humanKindLabel,
  groupSecondary,
} from './ArtifactPanel'
import type { ArtifactSummary, ArtifactRevisionInfo } from '../api'

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

// Fixture helpers: the panel's selection/labeling logic is pure, so it
// gets its own tests without a DOM (frontend-design: node-env, logic-only).
const summary = (over: Partial<ArtifactSummary>): ArtifactSummary => ({
  name: 'text:plan',
  kind: 'text',
  class: 'blob',
  latest_revision: 1,
  revisions: [],
  ...over,
})
const rev = (n: number, over?: Partial<ArtifactRevisionInfo>): ArtifactRevisionInfo => ({
  revision: n,
  mime_type: 'text/markdown',
  size: 1,
  ...over,
})

describe('selectPrimaryOutput', () => {
  it('returns null when the node has only judge rounds (or nothing)', () => {
    expect(selectPrimaryOutput([])).toBeNull()
    expect(selectPrimaryOutput([summary({ name: 'judge_round:t1-n1-1', kind: 'judge_round' })])).toBeNull()
  })

  it('prefers the artifact whose kind is the node\'s declared output', () => {
    const plan = summary({ name: 'text:plan', kind: 'text', latest_revision: 1 })
    const strayDoc = summary({ name: 'document:spec', kind: 'document', latest_revision: 9 })
    // The document is NEWER, but the node declared `text` - the plan wins.
    expect(selectPrimaryOutput([strayDoc, plan], 'text')?.name).toBe('text:plan')
  })

  it('falls back to the highest latest_revision when no kind is declared', () => {
    const older = summary({ name: 'text:plan', kind: 'text', latest_revision: 1 })
    const newer = summary({ name: 'document:spec', kind: 'document', latest_revision: 3 })
    expect(selectPrimaryOutput([older, newer])?.name).toBe('document:spec')
    expect(selectPrimaryOutput([older, newer], 'bytes')?.name).toBe('document:spec') // declared kind absent -> fallback
  })

  it('breaks latest_revision ties on lineage saved_at, then name', () => {
    const earlier = summary({ name: 'text:a', kind: 'text', latest_revision: 2, lineage: { saved_at: '2026-09-04T09:00:00Z' } })
    const later = summary({ name: 'text:b', kind: 'text', latest_revision: 2, lineage: { saved_at: '2026-09-04T10:00:00Z' } })
    expect(selectPrimaryOutput([earlier, later])?.name).toBe('text:b')
    const tie = summary({ name: 'text:a', kind: 'text', latest_revision: 2, lineage: { saved_at: '2026-09-04T09:00:00Z' } })
    const other = summary({ name: 'text:b', kind: 'text', latest_revision: 2, lineage: { saved_at: '2026-09-04T09:00:00Z' } })
    expect(selectPrimaryOutput([other, tie])?.name).toBe('text:a')
  })
})

describe('resolveScoredRevision', () => {
  it('resolves the primary artifact\'s revision from the round\'s scored list', () => {
    const body = {
      round: 1,
      passed: false,
      score: 0.42,
      scored: [
        { artifact_id: 'finding:692b00ee', revision: 3 },
        { artifact_id: 'text:plan', revision: 1 },
      ],
    }
    expect(resolveScoredRevision(body, 'text:plan', 2)).toBe(1)
  })

  it('falls back to the artifact\'s latest revision when the round did not score it', () => {
    const body = { round: 2, passed: true, score: 0.81, scored: [{ artifact_id: 'finding:692b00ee', revision: 3 }] }
    expect(resolveScoredRevision(body, 'text:plan', 2)).toBe(2)
    expect(resolveScoredRevision({ round: 1 }, 'text:plan', 4)).toBe(4)
  })
})

describe('toAscending', () => {
  it('reverses the endpoint\'s newest-first ordering without mutating it', () => {
    const newestFirst = [rev(3), rev(2), rev(1)]
    const asc = toAscending(newestFirst)
    expect(asc.map(r => r.revision)).toEqual([1, 2, 3])
    expect(newestFirst.map(r => r.revision)).toEqual([3, 2, 1])
  })
})

describe('humanKindLabel', () => {
  it('maps the known kinds to their human labels', () => {
    expect(humanKindLabel('finding')).toBe('Findings')
    expect(humanKindLabel('code_review')).toBe('Review')
    expect(humanKindLabel('pr_body')).toBe('PR description')
    expect(humanKindLabel('document')).toBe('Document')
    expect(humanKindLabel('text')).toBe('Files')
    expect(humanKindLabel('bytes')).toBe('Files')
  })

  it('capitalizes an unknown kind and names nothing for an absent one', () => {
    expect(humanKindLabel('workflow_step')).toBe('Workflow_step')
    expect(humanKindLabel(undefined)).toBe('Other')
  })
})

describe('groupSecondary', () => {
  it('groups by display label with first-seen order and per-group counts', () => {
    const groups = groupSecondary([
      summary({ name: 'finding:aa11', kind: 'finding' }),
      summary({ name: 'code_review:pr:1', kind: 'code_review' }),
      summary({ name: 'finding:bb22', kind: 'finding' }),
      summary({ name: 'text:notes-1', kind: 'text' }),
    ])
    expect(groups.map(g => g.label)).toEqual(['Findings', 'Review', 'Files'])
    expect(groups.map(g => g.items.length)).toEqual([2, 1, 1])
    expect(groups[0].items.map(a => a.name)).toEqual(['finding:aa11', 'finding:bb22'])
  })

  it('returns no groups for an empty input', () => {
    expect(groupSecondary([])).toEqual([])
  })
})
