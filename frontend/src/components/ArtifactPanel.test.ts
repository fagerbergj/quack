import { describe, it, expect } from 'vitest'
import {
  anchorNotes,
  selectPrimaryOutput,
  resolveScoredRevision,
  toAscending,
  isBookkeeping,
  artifactTitle,
  firstLine,
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

  it('#1250: a focus hint wins over the declared kind and the newest-wins default', () => {
    const a = summary({ name: 'text:a', kind: 'text', latest_revision: 3 })
    const b = summary({ name: 'text:b', kind: 'text', latest_revision: 1 })
    expect(selectPrimaryOutput([a, b], 'text', 'text:b')?.name).toBe('text:b')
  })

  it('#1250: an unrecognised focus hint is ignored - default selection still applies', () => {
    const a = summary({ name: 'text:a', kind: 'text', latest_revision: 3 })
    const b = summary({ name: 'text:b', kind: 'text', latest_revision: 1 })
    expect(selectPrimaryOutput([a, b], undefined, 'text:not-on-this-node')?.name).toBe('text:a')
  })

  // dag_node/dag_plan/bytes:* are never the primary, however
  // new their revision - a review always outranks everything else.
  it('never picks bookkeeping (dag_node, dag_plan, bytes:*), however new', () => {
    const dagNode = summary({ name: 'dag_node:n1', kind: 'dag_node', latest_revision: 9 })
    const dagPlan = summary({ name: 'dag_plan:main', kind: 'dag_plan', latest_revision: 9 })
    const bytes = summary({ name: 'bytes:issue', kind: 'bytes', latest_revision: 9 })
    const finding = summary({ name: 'finding:a', kind: 'finding', latest_revision: 1 })
    expect(selectPrimaryOutput([dagNode, dagPlan, bytes, finding])?.name).toBe('finding:a')
    expect(selectPrimaryOutput([dagNode, dagPlan, bytes])).toBeNull()
  })

  it('ranks code_review above the declared kind, a blob, and a finding, regardless of revision', () => {
    const review = summary({ name: 'code_review:pr:1', kind: 'code_review', class: 'structured', latest_revision: 1 })
    const declared = summary({ name: 'text:plan', kind: 'text', class: 'blob', latest_revision: 9 })
    const finding = summary({ name: 'finding:a', kind: 'finding', class: 'structured', latest_revision: 9 })
    expect(selectPrimaryOutput([declared, finding, review], 'text')?.name).toBe('code_review:pr:1')
  })

  it('ranks an answer/markdown blob above a finding when neither is the declared kind', () => {
    const answer = summary({ name: 'text:answer', kind: 'answer', class: 'blob', latest_revision: 1 })
    const finding = summary({ name: 'finding:a', kind: 'finding', class: 'structured', latest_revision: 9 })
    expect(selectPrimaryOutput([finding, answer])?.name).toBe('text:answer')
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

describe('isBookkeeping', () => {
  it('excludes dag_node, dag_plan, and any bytes:* name', () => {
    expect(isBookkeeping({ kind: 'dag_node', name: 'dag_node:n1' })).toBe(true)
    expect(isBookkeeping({ kind: 'dag_plan', name: 'dag_plan:main' })).toBe(true)
    expect(isBookkeeping({ kind: 'bytes', name: 'bytes:issue' })).toBe(true)
  })

  it('keeps everything else, including an unlisted kind like delivery_record', () => {
    expect(isBookkeeping({ kind: 'finding', name: 'finding:a' })).toBe(false)
    expect(isBookkeeping({ kind: 'delivery_record', name: 'delivery_record:pr:1' })).toBe(false)
  })
})

// Titles, never raw ids, everywhere an artifact is named.
describe('artifactTitle', () => {
  it('titles a review as verdict + finding count', () => {
    const s = summary({ name: 'code_review:pr:1464', kind: 'code_review', class: 'structured' })
    expect(artifactTitle(s, { verdict: 'approve', finding_ids: ['a', 'b', 'c', 'd'] })).toBe('Review · approve · 4 findings')
    expect(artifactTitle(s, { verdict: 'approve', finding_ids: ['a'] })).toBe('Review · approve · 1 finding')
  })

  it('titles a finding as severity + path:line', () => {
    const s = summary({ name: 'finding:76f9df59', kind: 'finding', class: 'structured' })
    expect(artifactTitle(s, { severity: 'nit', path: 'internal/acp/environment_golden_test.go', line_hint: 84 }))
      .toBe('nit · internal/acp/environment_golden_test.go:84')
  })

  it('titles a judge round as round + score + verdict', () => {
    const s = summary({ name: 'judge_round:t1-n1-1', kind: 'judge_round', class: 'structured' })
    expect(artifactTitle(s, { round: 1, score: 0.92, passed: true })).toBe('Judge round 1 · 0.92 · passed')
  })

  it('falls back to a blob\'s first line, and to the artifact name otherwise', () => {
    const blob = summary({ name: 'text:answer', kind: 'answer', class: 'blob' })
    expect(artifactTitle(blob, '# Opened PR #1464\n\nmore text')).toBe('Opened PR #1464')
    const unknown = summary({ name: 'delivery_record:pr:1', kind: 'delivery_record', class: 'structured' })
    expect(artifactTitle(unknown, { outcome: 'delivered' })).toBe('delivery_record:pr:1')
  })
})

describe('firstLine', () => {
  it('strips heading markup and skips leading blank lines', () => {
    expect(firstLine('\n\n# Opened PR #1464\n\nmore text')).toBe('Opened PR #1464')
  })

  it('returns null for text with no non-blank line', () => {
    expect(firstLine('\n  \n')).toBeNull()
  })
})
