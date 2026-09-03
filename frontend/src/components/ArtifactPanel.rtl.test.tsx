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
  const judgeRound = JSON.stringify({
    round: 1,
    passed: false,
    notes: [
      { ref: { artifact_id: 'finding:abc123', revision: 1, snippet: 'x may be nil here' }, text: 'Needs a concrete repro.', criterion: 'evidence' },
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
    if (url.includes('/artifacts/judge_round:t1-1/revisions')) {
      return jsonResponse({ data: [{ revision: 1, mime_type: 'application/json', size: judgeRound.length, kind: 'judge_round', class: 'structured' }] })
    }
    if (url.includes('/artifacts/finding:abc123?revision=2')) return textResponse(findingV2)
    if (url.includes('/artifacts/finding:abc123?revision=1')) return textResponse(findingV1)
    if (url.includes('/artifacts/finding:abc123/diff')) return textResponse(`--- finding:abc123@1\n+++ finding:abc123@2\n@@ -1 +1 @@\n-${findingV1}\n+${findingV2}\n`)
    if (url.includes('/artifacts/judge_round:t1-1')) return textResponse(judgeRound)
    if (url.endsWith('/artifacts')) {
      return jsonResponse({
        data: [
          { name: 'finding:abc123', kind: 'finding', class: 'structured', latest_revision: 2, lineage: { node_id: 'reviewer-1', round: 1, author: 'worker' }, revisions: [] },
          { name: 'judge_round:t1-1', kind: 'judge_round', class: 'structured', latest_revision: 1, lineage: { node_id: 'reviewer-1', round: 1, author: 'judge' }, revisions: [] },
        ],
      })
    }
    return jsonResponse({ data: [] })
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
    const findingButton = await screen.findByRole('button', { name: 'finding' })
    await user.click(findingButton)

    // Revision picker appears, defaulting to the latest (r2).
    const revisionSelect = await screen.findByLabelText('Revision')
    await waitFor(() => expect((revisionSelect as HTMLSelectElement).value).toBe('2'))

    // Switch to revision 1 - the fixture's judge_round notes reference
    // revision 1, so they only anchor once it's the one on screen (the
    // component correctly shows nothing yet for r2, which has none).
    await user.selectOptions(revisionSelect, '1')
    await waitFor(() => expect((revisionSelect as HTMLSelectElement).value).toBe('1'))

    // The judge note's snippet ("x may be nil here") is on r1's rationale
    // line - it should render as a clickable highlighted line.
    const highlighted = await screen.findByRole('button', { name: /Judge note on line/ })
    await user.click(highlighted)
    expect(await screen.findByText('Needs a concrete repro.')).toBeTruthy()

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

    await user.click(await screen.findByRole('button', { name: 'finding' }))
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
})
