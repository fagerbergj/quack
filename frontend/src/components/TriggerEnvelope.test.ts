// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { renderToStaticMarkup } from 'react-dom/server'
import { parseEnvelope, commentsSummaryLabel, changedFilesSummaryLabel, checksSummaryLabel, accumulateComments, type EnvelopeBlock } from './envelope'
import { TriggerMessage } from './TriggerEnvelope'

// A full CI-fix envelope (design: .quack/trigger-prompts-v2.md, Step 6/7) -
// every known block type in envelope order.
const CI_FIX_ENVELOPE = `<permissions>push_commits_to_pr, join_pr_conversation</permissions>
<deliverable>commits on this PR's head branch that make the failing checks pass</deliverable>
<pull_request number="97">
  <title>Material 3 theming with dynamic color and dark mode</title>
  <description>Implements Material You theming.

Closes #65</description>
</pull_request>
<comments new="1" edited="0" deleted="0">[{"id":5181177234,"created_at":"2026-08-04T16:44:10Z","user":{"login":"fagerbergj"},"body":"the BAC status colors fail contrast - fix those"}]</comments>
<changed_files count="16" additions="880" deletions="330">[{"filename":"app/src/main/Theme.kt","additions":40,"deletions":5},{"filename":"app/src/main/Color.kt","additions":12,"deletions":2}]</changed_files>
<event name="workflow_run.completed">{"action":"completed","workflow_run":{"name":"build","conclusion":"failure","head_sha":"9c8d7e6"}}</event>
<context dir="/workspace/ctx-github-nightsout-97">
  <file name="check-runs.json">GET /repos/…/commits/9c8d7e6/check-runs?status=completed</file>
  <file name="files.json">GET /repos/…/pulls/97/files</file>
</context>`

describe('parseEnvelope', () => {
  it('parses a full envelope into ordered, typed blocks', () => {
    const blocks = parseEnvelope(CI_FIX_ENVELOPE)
    expect(blocks).not.toBeNull()
    expect(blocks!.map(b => b.kind)).toEqual([
      'permissions', 'deliverable', 'ask', 'comments', 'changed_files', 'event', 'context',
    ])
    const [permissions, deliverable, ask, comments, files, event, context] = blocks!
    expect(permissions).toMatchObject({ kind: 'permissions', text: 'push_commits_to_pr, join_pr_conversation' })
    expect(deliverable).toMatchObject({ kind: 'deliverable', text: "commits on this PR's head branch that make the failing checks pass" })
    expect(ask).toMatchObject({ kind: 'ask', askKind: 'pull_request', number: '97', title: 'Material 3 theming with dynamic color and dark mode' })
    if (ask.kind === 'ask') expect(ask.description).toContain('Closes #65')
    if (comments.kind === 'comments') {
      expect(comments.comments).toHaveLength(1)
      expect(comments.comments![0]).toMatchObject({ author: 'fagerbergj', id: '5181177234' })
      expect(commentsSummaryLabel(comments)).toBe('1 new, 0 edited, 0 deleted')
    }
    if (files.kind === 'changed_files') {
      expect(files.files).toHaveLength(2)
      expect(changedFilesSummaryLabel(files)).toBe('16 files, +880/-330')
    }
    if (event.kind === 'event') {
      expect(event.name).toBe('workflow_run.completed')
      expect(event.pretty).toContain('"conclusion": "failure"')
    }
    if (context.kind === 'context') {
      expect(context.dir).toBe('/workspace/ctx-github-nightsout-97')
      expect(context.files).toEqual([
        { name: 'check-runs.json', endpoint: 'GET /repos/…/commits/9c8d7e6/check-runs?status=completed' },
        { name: 'files.json', endpoint: 'GET /repos/…/pulls/97/files' },
      ])
    }
  })

  it('keeps an unrecognised block instead of dropping it', () => {
    const withUnknown = CI_FIX_ENVELOPE.replace(
      '</context>',
      '</context>\n<workspace_summary lines="3">3 files touched in internal/theme</workspace_summary>',
    )
    const blocks = parseEnvelope(withUnknown)
    const unknown = blocks!.find(b => b.kind === 'unknown')
    expect(unknown).toMatchObject({ kind: 'unknown', tag: 'workspace_summary', raw: '3 files touched in internal/theme' })
  })

  it('returns null for a plain chat message (no envelope markers)', () => {
    expect(parseEnvelope('Please add dark mode support.')).toBeNull()
    // Starts with "<" but isn't envelope-shaped - still falls back.
    expect(parseEnvelope('<3 this feature, ship it')).toBeNull()
  })

  it('degrades a comments block with non-JSON content to the raw fallback, not a throw', () => {
    const bad = '<permissions>p</permissions><deliverable>d</deliverable><comments count="1">not json</comments>'
    const blocks = parseEnvelope(bad)
    const comments = blocks!.find(b => b.kind === 'comments')
    expect(comments).toMatchObject({ kind: 'comments', comments: null, raw: 'not json' })
  })

  it('never throws on malformed/unterminated tags', () => {
    const brokenInputs = [
      '<permissions>join_issue_conversation<deliverable>a plan</deliverable>', // unterminated permissions
      '<<<not a tag at all',
      '<permissions',
      '',
      '   ',
    ]
    for (const input of brokenInputs) {
      expect(() => parseEnvelope(input)).not.toThrow()
    }
  })

  it('unwraps a <trigger> wrapper around permissions/deliverable/event/context', () => {
    const wrapped = `<issue number="65"><title>t</title><description>d</description></issue>
<trigger>
  <permissions>join_issue_conversation</permissions>
  <deliverable>an implementation plan</deliverable>
  <event name="issues.labeled">{"action":"labeled"}</event>
  <context dir="/workspace/ctx-1"/>
</trigger>`
    const blocks = parseEnvelope(wrapped)
    expect(blocks!.map(b => b.kind)).toEqual(['ask', 'permissions', 'deliverable', 'event', 'context'])
  })
})

// A <checks> block (quack-extensions/github's checksBlock format): one
// "name: status[ conclusion]" line per check plus a count/summary attribute pair.
const CHECKS_ENVELOPE = `<permissions>p</permissions>
<deliverable>d</deliverable>
<checks count="3" summary="1 failing, 1 pending, 1 passing">build: completed failure
lint: in_progress
test: completed success
</checks>`

function checksBlockOf(content: string) {
  const b = parseEnvelope(content)!.find(b => b.kind === 'checks')
  if (!b) throw new Error('fixture has no <checks> block')
  return b as Extract<EnvelopeBlock, { kind: 'checks' }>
}

describe('checksSummaryLabel', () => {
  it('uses the summary attribute when present', () => {
    const b = checksBlockOf(CHECKS_ENVELOPE)
    expect(b.checks).toEqual([
      { name: 'build', status: 'completed', conclusion: 'failure' },
      { name: 'lint', status: 'in_progress' },
      { name: 'test', status: 'completed', conclusion: 'success' },
    ])
    expect(checksSummaryLabel(b)).toBe('checks: 1 failing, 1 pending, 1 passing')
  })

  it('falls back to a plain count when the summary attribute is absent', () => {
    const noSummary = `<permissions>p</permissions><deliverable>d</deliverable><checks count="2">build: completed success
lint: completed success
</checks>`
    expect(checksSummaryLabel(checksBlockOf(noSummary))).toBe('2 checks')
  })

  it('falls back to a plain count when the summary attribute is empty/malformed', () => {
    const emptySummary = `<permissions>p</permissions><deliverable>d</deliverable><checks count="1" summary="">build: completed success
</checks>`
    expect(checksSummaryLabel(checksBlockOf(emptySummary))).toBe('1 check')
  })

  it('degrades a checks block with an unparsable line to the raw fallback, not a throw', () => {
    const malformed = `<permissions>p</permissions><deliverable>d</deliverable><checks count="1" summary="1 passing">not a valid check line</checks>`
    const b = checksBlockOf(malformed)
    expect(b.checks).toBeNull()
    expect(b.raw).toBe('not a valid check line')
    // The summary attribute still parses independently of the body.
    expect(checksSummaryLabel(b)).toBe('checks: 1 passing')
  })
})

// #730 fixtures: a seed turn (full snapshot, envelope.go's commentsBlock with
// gh.delta == nil) followed by two resume deltas, mirroring a real
// issue-comment back-and-forth across three triggers.
const SEED_TURN = `<permissions>join_issue_conversation</permissions>
<deliverable>an answer to their message</deliverable>
<comments count="2">${JSON.stringify([
  { id: 1, created_at: '2026-08-01T10:00:00Z', user: { login: 'alice' }, body: 'Alpha: first question' },
  { id: 2, created_at: '2026-08-01T10:05:00Z', user: { login: 'bob' }, body: 'Bravo: second question' },
])}</comments>`

const TURN2_DELTA = `<permissions>join_issue_conversation</permissions>
<deliverable>an answer to their message</deliverable>
<comments new="1" edited="0" deleted="0">${JSON.stringify([
  { id: 3, created_at: '2026-08-01T11:00:00Z', user: { login: 'carol' }, body: 'Charlie: follow-up', quack_status: 'new' },
])}</comments>`

const TURN3_DELTA = `<permissions>join_issue_conversation</permissions>
<deliverable>an answer to their message</deliverable>
<comments new="1" edited="0" deleted="0">${JSON.stringify([
  { id: 4, created_at: '2026-08-01T12:00:00Z', user: { login: 'dave' }, body: 'Delta: another follow-up', quack_status: 'new' },
])}</comments>`

function commentsBlock(content: string) {
  const b = parseEnvelope(content)!.find(b => b.kind === 'comments')
  if (!b) throw new Error('fixture has no <comments> block')
  return b as Extract<EnvelopeBlock, { kind: 'comments' }>
}

describe('accumulateComments', () => {
  it('folds new deltas onto the seed in arrival order (test case 1)', () => {
    const acc = accumulateComments([SEED_TURN, TURN2_DELTA], commentsBlock(TURN3_DELTA))
    expect(acc.complete).toBe(true)
    expect(acc.comments.map(c => c.body)).toEqual([
      'Alpha: first question', 'Bravo: second question', 'Charlie: follow-up', 'Delta: another follow-up',
    ])
    // Turn 3's own collapsed header still reports just its own delta, not the total.
    expect(commentsSummaryLabel(commentsBlock(TURN3_DELTA))).toBe('1 new, 0 edited, 0 deleted')
  })

  it('replaces an edited comment by id and drops a deleted one (test case 2)', () => {
    const editAndDelete = `<permissions>p</permissions><deliverable>d</deliverable><comments new="0" edited="1" deleted="1">${JSON.stringify([
      { id: 2, created_at: '2026-08-01T13:00:00Z', user: { login: 'bob' }, body: 'Bravo: revised', quack_status: 'edited' },
      { id: 1, created_at: '2026-08-01T10:00:00Z', user: { login: 'alice' }, body: 'Alpha: first question', quack_status: 'deleted' },
    ])}</comments>`
    const acc = accumulateComments([SEED_TURN], commentsBlock(editAndDelete))
    expect(acc.complete).toBe(true)
    expect(acc.comments).toHaveLength(1)
    expect(acc.comments[0]).toMatchObject({ id: '2', body: 'Bravo: revised' })
  })

  it('reports an incomplete view when no seed is visible (test case 3)', () => {
    const acc = accumulateComments([], commentsBlock(TURN2_DELTA))
    expect(acc.complete).toBe(false)
    // What IS known is still surfaced - the incompleteness is a flag, not a blackout.
    expect(acc.comments.map(c => c.body)).toEqual(['Charlie: follow-up'])
  })

  it('surfaces a deleted comment it never saw alive instead of dropping it (no seed in view)', () => {
    const deleteOnly = `<permissions>p</permissions><deliverable>d</deliverable><comments new="0" edited="0" deleted="1">${JSON.stringify([
      { id: 1, created_at: '2026-08-01T10:00:00Z', user: { login: 'alice' }, body: 'Alpha: first question', quack_status: 'deleted' },
    ])}</comments>`
    const acc = accumulateComments([], commentsBlock(deleteOnly))
    expect(acc.complete).toBe(false)
    expect(acc.comments).toHaveLength(1)
    expect(acc.comments[0]).toMatchObject({ id: '1', quackStatus: 'deleted' })
  })

  it('stays complete and empty for a seed turn with zero comments', () => {
    const emptySeed = `<permissions>p</permissions><deliverable>d</deliverable><comments count="0">[]</comments>`
    const acc = accumulateComments([], commentsBlock(emptySeed))
    expect(acc).toEqual({ comments: [], complete: true })
  })
})

describe('TriggerMessage', () => {
  it('renders a full envelope with permissions/deliverable/title expanded and the rest collapsed', () => {
    const out = renderToStaticMarkup(createElement(TriggerMessage, { content: CI_FIX_ENVELOPE }))
    expect(out).toContain('Permissions:')
    expect(out).toContain('push_commits_to_pr, join_pr_conversation')
    expect(out).toContain('Deliverable:')
    expect(out).toContain('Material 3 theming with dynamic color and dark mode')
    // Ask (title/description) is not behind a <details> disclosure.
    expect(out).not.toMatch(/<summary[^>]*>[^<]*Material 3 theming/)
    // The collapsed sections exist but none carry the `open` attribute.
    const detailsOpenTags = out.match(/<details[^>]*>/g) ?? []
    expect(detailsOpenTags.length).toBeGreaterThanOrEqual(4) // comments, changed_files, event, context
    for (const tag of detailsOpenTags) expect(tag).not.toContain('open')
    expect(out).toContain('1 new, 0 edited, 0 deleted')
    expect(out).toContain('16 files, +880/-330')
    expect(out).toContain('workflow_run.completed')
  })

  it('renders a <checks> block with the summary label collapsed and per-check lines expanded', () => {
    const withChecks = CI_FIX_ENVELOPE.replace('</changed_files>', '</changed_files>\n' + CHECKS_ENVELOPE.split('\n').slice(2).join('\n'))
    const out = renderToStaticMarkup(createElement(TriggerMessage, { content: withChecks }))
    expect(out).toContain('checks: 1 failing, 1 pending, 1 passing')
    expect(out).toContain('build')
    expect(out).toContain('failure')
  })

  it('renders an unknown block as a labelled collapsed section rather than dropping it', () => {
    const withUnknown = CI_FIX_ENVELOPE.replace(
      '</context>',
      '</context>\n<workspace_summary lines="3">3 files touched in internal/theme</workspace_summary>',
    )
    const out = renderToStaticMarkup(createElement(TriggerMessage, { content: withUnknown }))
    expect(out).toContain('workspace_summary')
    expect(out).toContain('3 files touched in internal/theme')
  })

  it('renders a plain-string message exactly as today (no envelope detected)', () => {
    const out = renderToStaticMarkup(createElement(TriggerMessage, { content: 'Please add dark mode support.' }))
    expect(out).toContain('bg-blue-600')
    expect(out).toContain('Please add dark mode support.')
    expect(out).not.toContain('<details')
  })

  it('renders the accumulated comment history, not just this turn\'s own slice (#730)', () => {
    const out = renderToStaticMarkup(createElement(TriggerMessage, { content: TURN3_DELTA, priorContents: [SEED_TURN, TURN2_DELTA] }))
    // The header still reports this turn's own delta only.
    expect(out).toContain('1 new, 0 edited, 0 deleted')
    // But the body shows the whole folded history.
    expect(out).toContain('Alpha: first question')
    expect(out).toContain('Bravo: second question')
    expect(out).toContain('Charlie: follow-up')
    expect(out).toContain('Delta: another follow-up')
    expect(out).not.toContain('Incomplete history')
  })

  it('renders an explicit incomplete-history notice when no seed is in the visible window (#730)', () => {
    const out = renderToStaticMarkup(createElement(TriggerMessage, { content: TURN2_DELTA }))
    expect(out).toContain('Incomplete history')
    // The delta comment it DOES know about is still shown, just flagged.
    expect(out).toContain('Charlie: follow-up')
  })

  it('a 40-comment thread stays collapsed by default, not blowing out message height', () => {
    const comments = Array.from({ length: 40 }, (_, i) => ({
      id: i,
      created_at: '2026-08-04T00:00:00Z',
      user: { login: `user${i}` },
      body: `Comment number ${i}`,
    }))
    const envelope = `<permissions>join_pr_conversation</permissions>
<deliverable>a review</deliverable>
<comments count="40">${JSON.stringify(comments)}</comments>`
    const out = renderToStaticMarkup(createElement(TriggerMessage, { content: envelope }))
    expect(out).toContain('40 comments')
    const detailsTags = out.match(/<details[^>]*>/g) ?? []
    expect(detailsTags.length).toBeGreaterThan(0)
    for (const tag of detailsTags) expect(tag).not.toContain('open') // collapsed by default
  })
})

// renderToStaticMarkup proves the collapsed markup exists; it never runs a
// click. These mount into real jsdom (Expandable.test.ts's pattern - no
// testing-library in this repo) and dispatch an actual click on <summary>, so
// what's asserted is the native <details> `open` toggle actually firing, the
// same mechanism the browser's UA stylesheet uses to show/hide the content.
describe('TriggerMessage interaction (real DOM, not string assertions)', () => {
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
  })

  function mount(content: string) {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    act(() => root!.render(createElement(TriggerMessage, { content })))
    return host
  }

  it('a click on <summary> actually opens its <details>, not just markup that claims to be collapsible', () => {
    const el = mount(CI_FIX_ENVELOPE)
    const details = el.querySelectorAll('details')
    expect(details.length).toBeGreaterThanOrEqual(4)
    for (const d of details) expect(d.open).toBe(false)
    const commentsDetails = [...details].find(d => d.querySelector('summary')?.textContent?.includes('new'))!
    act(() => commentsDetails.querySelector('summary')!.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true })))
    expect(commentsDetails.open).toBe(true)
    // Only the clicked section opened - the others stay collapsed.
    for (const d of details) if (d !== commentsDetails) expect(d.open).toBe(false)
  })

  it('never throws when actually mounted (layout effects run) on malformed input', () => {
    const brokenInputs = [
      '<permissions>join_issue_conversation<deliverable>a plan</deliverable>',
      '<<<not a tag at all',
      '<permissions',
      '<comments count="1">not json</comments>',
    ]
    for (const input of brokenInputs) {
      expect(() => mount(input)).not.toThrow()
      act(() => root!.unmount())
      host!.remove()
    }
  })

  it('a failing check is visibly distinguishable from a passing/pending one once its section is opened', () => {
    const envelope = `<permissions>join_pr_conversation</permissions>
<deliverable>a review</deliverable>
${CHECKS_ENVELOPE.split('\n').slice(2).join('\n')}`
    const el = mount(envelope)
    const details = el.querySelector('details')!
    act(() => details.querySelector('summary')!.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true })))
    expect(details.open).toBe(true)
    expect(details.textContent).toContain('build')
    expect(details.textContent).toContain('failure')
    // The failing check's danger styling, not shared by the passing/pending rows.
    expect(details.innerHTML).toMatch(/text-red-600[^"]*"[^>]*>build/)
  })

  it('a deleted comment is visibly distinguishable from a live one once its section is opened', () => {
    const envelope = `<permissions>join_pr_conversation</permissions>
<deliverable>a review</deliverable>
<comments new="0" edited="0" deleted="1">${JSON.stringify([
      { id: 1, created_at: '2026-08-04T00:00:00Z', user: { login: 'carol' }, body: 'retracted statement' },
    ].map(c => ({ ...c, quack_status: 'deleted' })))}</comments>`
    const el = mount(envelope)
    const details = el.querySelector('details')!
    act(() => details.querySelector('summary')!.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true })))
    expect(details.open).toBe(true)
    expect(details.textContent).toContain('deleted')
    // Ignoring quack_status entirely would render this identically to a live comment.
    expect(details.innerHTML).toMatch(/line-through/)
  })
})
