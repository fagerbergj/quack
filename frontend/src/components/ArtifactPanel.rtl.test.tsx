// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ArtifactPanel } from './ArtifactPanel'
import { client } from '../generated/client.gen'

afterEach(cleanup)

// stubFetch mirrors ArtifactPanel.stories.tsx's fixture: no MSW in this repo
// (see frontend-design skill - reach for native/existing before a new
// dependency), and the panel's whole network surface is the REST client in
// ../api.ts, so a plain global.fetch stub covers it.
function stubFetch() {
  const findingV1 = JSON.stringify({ path: 'a.go', title: 'missing nil check', rationale: 'x may be nil here' })
  const findingV2 = JSON.stringify({ path: 'a.go', title: 'missing nil check (fixed)', rationale: 'x may be nil here' })
  // finding:other has only ONE revision - the review's exact repro shape
  // (artifact A has revisions {2, 1}, artifact B has only 1) for the stale
  // 404-banner bug: switching A -> B used to fetch B?revision=2 (A's stale
  // selectedRev) before B's own revisions effect settled.
  const otherV1 = JSON.stringify({ path: 'b.go', title: 'unused import', rationale: 'b is never read' })
  const judgeRound = JSON.stringify({
    round: 1,
    passed: false,
    notes: [
      { ref: { artifact_id: 'finding:abc123', revision: 1, snippet: 'x may be nil here' }, text: 'Needs a concrete repro.', criterion: 'evidence' },
      // Same line as the note above (both match "x may be nil here") -
      // review finding 5: each note on a shared line must be its own
      // reachable/announced control, not just notes[0].
      { ref: { artifact_id: 'finding:abc123', revision: 1, snippet: 'nil here' }, text: 'Also flag the caller.', criterion: 'coverage' },
      { ref: { artifact_id: 'finding:abc123', revision: 1, line_hint: 999 }, text: 'Unanchored fixture note.' },
    ],
  })

  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const url = decodeURIComponent(input instanceof Request ? input.url : String(input))
    if (url.includes('/artifacts/finding:abc123/revisions')) {
      return jsonResponse({
        data: [
          { revision: 2, mime_type: 'application/json', size: findingV2.length, kind: 'finding', class: 'structured', lineage: { node_id: 'reviewer-1', round: 1, author: 'worker' } },
          { revision: 1, mime_type: 'application/json', size: findingV1.length, kind: 'finding', class: 'structured', lineage: { node_id: 'reviewer-1', round: 1, author: 'worker' } },
        ],
      })
    }
    if (url.includes('/artifacts/finding:other/revisions')) {
      return jsonResponse({
        data: [
          { revision: 1, mime_type: 'application/json', size: otherV1.length, kind: 'finding', class: 'structured', lineage: { node_id: 'reviewer-1', round: 1, author: 'worker' } },
        ],
      })
    }
    if (url.includes('/artifacts/judge_round:t1-1/revisions')) {
      return jsonResponse({ data: [{ revision: 1, mime_type: 'application/json', size: judgeRound.length, kind: 'judge_round', class: 'structured' }] })
    }
    if (url.includes('/artifacts/finding:abc123?revision=2')) return textResponse(findingV2)
    if (url.includes('/artifacts/finding:abc123?revision=1')) return textResponse(findingV1)
    if (url.includes('/artifacts/finding:other?revision=1')) return textResponse(otherV1)
    // NOT ?revision=2 for finding:other - it has only revision 1, so a
    // request for revision 2 (the stale-selectedRev bug) 404s, matching the
    // real server's GetChatArtifact.
    if (url.includes('/artifacts/finding:abc123/diff')) return textResponse(`--- finding:abc123@1\n+++ finding:abc123@2\n@@ -1 +1 @@\n-${findingV1}\n+${findingV2}\n`)
    if (url.includes('/artifacts/judge_round:t1-1')) return textResponse(judgeRound)
    if (url.endsWith('/artifacts')) {
      return jsonResponse({
        data: [
          { name: 'finding:abc123', kind: 'finding', class: 'structured', latest_revision: 2, lineage: { node_id: 'reviewer-1', round: 1, author: 'worker' }, revisions: [] },
          { name: 'finding:other', kind: 'finding', class: 'structured', latest_revision: 1, lineage: { node_id: 'reviewer-1', round: 1, author: 'worker' }, revisions: [] },
          { name: 'judge_round:t1-1', kind: 'judge_round', class: 'structured', latest_revision: 1, lineage: { node_id: 'reviewer-1', round: 1, author: 'judge' }, revisions: [] },
        ],
      })
    }
    return new Response(JSON.stringify({ error: 'not found' }), { status: 404, headers: { 'Content-Type': 'application/json' } })
  }))
}

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
}
function textResponse(body: string): Response {
  return new Response(body, { status: 200, headers: { 'Content-Type': 'text/plain' } })
}

// jsdom has no HTMLDialogElement.showModal/close - stub them so the panel's
// mount effect doesn't throw; behaviour (open/close) isn't under test here,
// RTL just needs the dialog's children to render.
beforeEach(() => {
  // jsdom doesn't implement <dialog> at all (no showModal/close, no `open`
  // reflection) - stub both, and set `open` on showModal, because RTL's
  // getByRole treats a dialog with no `open` attribute as closed/hidden
  // (correctly mirroring a real browser), which would hide everything
  // inside it from every query below.
  HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) { this.setAttribute('open', '') }
  HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) { this.removeAttribute('open') }
  // The generated client builds `new Request(url, ...)` itself before ever
  // calling fetch (generated/client/client.gen.ts) - Node's Request/undici
  // (what jsdom's global fetch actually is) can't resolve the app's relative
  // baseUrl ('/') without a document to anchor it against, so it throws
  // before the stubbed fetch below is ever reached. Absolute base, test-only.
  client.setConfig({ baseUrl: 'http://localhost' })
  stubFetch()
})

describe('ArtifactPanel', () => {
  it('lists artifacts, switches revision, toggles diff, anchors a judge note by click, and lists unanchored notes', async () => {
    const user = userEvent.setup()
    render(<ArtifactPanel chatId="chat-1" nodeId="reviewer-1" onClose={() => {}} />)

    // Select the finding artifact from the Outputs group.
    const findingButton = (await screen.findAllByRole('button', { name: 'finding' }))[0]
    await user.click(findingButton)

    // Revision picker appears, defaulting to the latest (r2).
    const revisionSelect = await screen.findByLabelText('Revision')
    await waitFor(() => expect((revisionSelect as HTMLSelectElement).value).toBe('2'))

    // Switch to revision 1 - the fixture's judge_round notes reference
    // revision 1, so they only anchor once it's the one on screen (the
    // component correctly shows nothing yet for r2, which has none).
    await user.selectOptions(revisionSelect, '1')
    await waitFor(() => expect((revisionSelect as HTMLSelectElement).value).toBe('1'))

    // Both notes anchor to r1's rationale line ("x may be nil here" and "nil
    // here" both match it) - each must be its OWN reachable/announced
    // control (review finding 5), not just the first.
    const lineNotes = await screen.findAllByRole('button', { name: /Judge note on line/ })
    expect(lineNotes).toHaveLength(2)
    await user.click(lineNotes[0])
    expect(await screen.findByText('Needs a concrete repro.')).toBeTruthy()
    await user.click(lineNotes[1])
    expect(await screen.findByText('Also flag the caller.')).toBeTruthy()

    // The unanchored note (bad line_hint, no snippet match) lists separately.
    expect(await screen.findByText('Unanchored fixture note.')).toBeTruthy()
    expect(screen.getByText('Unanchored notes')).toBeTruthy()

    // Turn on diff mode and pick revision 2 to diff against.
    const diffCheckbox = screen.getByRole('checkbox', { name: 'Diff' })
    await user.click(diffCheckbox)
    const against = await screen.findByLabelText('Diff against revision')
    await user.selectOptions(against, '2')

    expect(await screen.findByText(/-\{"path":"a.go","title":"missing nil check"/)).toBeTruthy()
  })

  it('shows a one-line message instead of a blank pane when diffing a revision against itself', async () => {
    const user = userEvent.setup()
    render(<ArtifactPanel chatId="chat-1" nodeId="reviewer-1" onClose={() => {}} />)

    await user.click((await screen.findAllByRole('button', { name: 'finding' }))[0])
    const revisionSelect = await screen.findByLabelText('Revision')
    await waitFor(() => expect((revisionSelect as HTMLSelectElement).value).toBe('2'))

    await user.click(screen.getByRole('checkbox', { name: 'Diff' }))
    // Switch the primary revision to 1 first so "against" 1 becomes reachable,
    // then flip it back to 2 to land both pickers on the same revision.
    await user.selectOptions(revisionSelect, '1')
    const against = await screen.findByLabelText('Diff against revision')
    await user.selectOptions(against, '2')
    await user.selectOptions(revisionSelect, '2')

    expect(await screen.findByText(/Pick a different revision to diff against/)).toBeTruthy()
  })

  // Review #1113's blocking finding: switching from an artifact with
  // revisions {2, 1} to one with only revision 1 used to fetch
  // `finding:other?revision=2` (the FIRST artifact's stale selectedRev) in
  // the same commit as the selection change, 404ing and leaving a red error
  // banner that nothing ever cleared - even once the second artifact's real
  // content rendered correctly underneath it.
  it('clears a previous artifact error banner when switching to a second artifact', async () => {
    const user = userEvent.setup()
    render(<ArtifactPanel chatId="chat-1" nodeId="reviewer-1" onClose={() => {}} />)

    const findingButtons = await screen.findAllByRole('button', { name: 'finding' })
    expect(findingButtons).toHaveLength(2)

    // Select A (finding:abc123, latest revision 2) first.
    await user.click(findingButtons[0])
    await waitFor(() => expect((screen.getByLabelText('Revision') as HTMLSelectElement).value).toBe('2'))

    // Switch to B (finding:other, only revision 1).
    await user.click(findingButtons[1])
    await waitFor(() => expect((screen.getByLabelText('Revision') as HTMLSelectElement).value).toBe('1'))
    expect(await screen.findByText(/unused import/)).toBeTruthy()

    // No stale error banner from the old selectedRev=2 -> B?revision=2 404.
    expect(screen.queryByText(/404/)).toBeNull()
    expect(screen.queryByText(/Fetch artifact failed/)).toBeNull()
  })
})
