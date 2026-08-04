import { describe, it, expect } from 'vitest'
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { parseEnvelope, commentsSummaryLabel, changedFilesSummaryLabel } from './envelope'
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
