// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ArtifactPanel } from './ArtifactPanel'
import { client } from '../generated/client.gen'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  document.documentElement.classList.remove('dark')
})

// The fixture is a FINISHED plan node (#1178's acceptance case a): one text
// output (text:plan, two revisions), two judge rounds (round 1 failed 0.42
// judging revision 1, round 2 passed 0.81 judging revision 2), a finding
// and a dispatch-authored code_review as "More" material, plus one
// artifact belonging to a DIFFERENT node that must never leak into this
// node's panel. No MSW in this repo (see frontend-design skill - reach for
// native/existing before a new dependency), and the panel's whole network
// surface is the REST client in ../api.ts, so a plain global.fetch stub
// covers it.
const PLAN_V1 = '# Plan v1\n\nThe apple pie recipe needs a citation.\n'
const PLAN_V2 = '# Plan v2\n\nThe apple pie recipe is now cited.\n'
const round1 = JSON.stringify({
  round: 1,
  passed: false,
  score: 0.42,
  scored: [{ artifact_id: 'text:plan', revision: 1 }],
  notes: [
    { ref: { artifact_id: 'text:plan', revision: 1, snippet: 'apple pie recipe' }, text: 'Cite the source.', criterion: 'evidence' },
  ],
})
const round2 = JSON.stringify({
  round: 2,
  passed: true,
  score: 0.81,
  scored: [{ artifact_id: 'text:plan', revision: 2 }],
  notes: [],
})
const findingV1 = JSON.stringify({ path: 'a.go', title: 'missing nil check', rationale: 'x may be nil here' })

function stubPlanFixture() {
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const url = decodeURIComponent(input instanceof Request ? input.url : String(input))
    // Revisions endpoints come back NEWEST-FIRST, like the real endpoint
    // (openapi.yaml's ArtifactRevisionList) - the panel reverses them.
    if (url.includes('/artifacts/text:plan/revisions')) {
      return jsonResponse({
        data: [
          { revision: 2, mime_type: 'text/markdown', size: PLAN_V2.length, kind: 'text', class: 'blob', lineage: { node_id: 'planner-1', round: 2, author: 'worker', saved_at: '2026-09-04T10:00:00Z' } },
          { revision: 1, mime_type: 'text/markdown', size: PLAN_V1.length, kind: 'text', class: 'blob', lineage: { node_id: 'planner-1', round: 1, author: 'worker', saved_at: '2026-09-04T09:00:00Z', trigger_annotation: 'judge_round:t1-planner-1-1' } },
        ],
      })
    }
    if (url.includes('/artifacts/finding:692b00ee/revisions')) {
      return jsonResponse({
        data: [
          { revision: 1, mime_type: 'application/json', size: findingV1.length, kind: 'finding', class: 'structured', lineage: { node_id: 'planner-1', round: 1, author: 'worker' } },
        ],
      })
    }
    if (url.includes('/artifacts/text:plan?revision=2')) return textResponse(PLAN_V2)
    if (url.includes('/artifacts/text:plan?revision=1')) return textResponse(PLAN_V1)
    if (url.includes('/artifacts/finding:692b00ee?revision=1')) return textResponse(findingV1)
    if (url.includes('/artifacts/text:plan/diff')) return textResponse(`--- text:plan@1\n+++ text:plan@2\n@@ -1,2 +1,2 @@\n-# Plan v1\n-Second line old.\n+# Plan v2\n+Second line new.\n`)
    // Judge bodies: the panel fetches each judge_round's LATEST content
    // (no ?revision) and parses it.
    if (url.includes('/artifacts/judge_round:t1-planner-1-1')) return textResponse(round1)
    if (url.includes('/artifacts/judge_round:t1-planner-1-2')) return textResponse(round2)
    if (url.endsWith('/artifacts')) {
      return jsonResponse({
        data: [
          { name: 'text:plan', kind: 'text', class: 'blob', latest_revision: 2, lineage: { node_id: 'planner-1', round: 2, author: 'worker', saved_at: '2026-09-04T10:00:00Z' }, revisions: [] },
          { name: 'finding:692b00ee', kind: 'finding', class: 'structured', latest_revision: 1, lineage: { node_id: 'planner-1', round: 1, author: 'worker' }, revisions: [] },
          { name: 'code_review:pr:1', kind: 'code_review', class: 'structured', latest_revision: 1, lineage: { node_id: 'planner-1', round: 1, author: 'dispatch' }, revisions: [] },
          { name: 'judge_round:t1-planner-1-1', kind: 'judge_round', class: 'structured', latest_revision: 1, lineage: { node_id: 'planner-1', author: 'judge' }, revisions: [] },
          { name: 'judge_round:t1-planner-1-2', kind: 'judge_round', class: 'structured', latest_revision: 1, lineage: { node_id: 'planner-1', author: 'judge' }, revisions: [] },
          // A different node's artifact - never part of this panel.
          { name: 'text:other-node', kind: 'text', class: 'blob', latest_revision: 1, lineage: { node_id: 'writer-1', author: 'worker' }, revisions: [] },
        ],
      })
    }
    return new Response(JSON.stringify({ error: 'not found' }), { status: 404, headers: { 'Content-Type': 'application/json' } })
  }))
}

// The 413 fixture (#1178 requirement 3): a large text artifact whose
// diff pair the server rejects with 413 over the 256KB bound. The server's
// own message embeds the artifact id - the panel must show ITS OWN reason
// instead.
function stubLargeFixture() {
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const url = decodeURIComponent(input instanceof Request ? input.url : String(input))
    if (url.includes('/artifacts/text:big/revisions')) {
      return jsonResponse({
        data: [
          { revision: 2, mime_type: 'text/markdown', size: 300000, kind: 'text', class: 'blob', lineage: { node_id: 'writer-1', author: 'worker' } },
          { revision: 1, mime_type: 'text/markdown', size: 300000, kind: 'text', class: 'blob', lineage: { node_id: 'writer-1', author: 'worker' } },
        ],
      })
    }
    if (url.includes('/artifacts/text:big?revision=2')) return textResponse('# Big two\n')
    if (url.includes('/artifacts/text:big?revision=1')) return textResponse('# Big one\n')
    if (url.includes('/artifacts/text:big/diff')) {
      return jsonResponse({
        error: 'revision exceeds the 262144 byte diff limit; fetch it directly via GET .../artifacts/text:big instead',
      }, { status: 413 })
    }
    if (url.endsWith('/artifacts')) {
      return jsonResponse({
        data: [
          { name: 'text:big', kind: 'text', class: 'blob', latest_revision: 2, lineage: { node_id: 'writer-1', author: 'worker' }, revisions: [] },
        ],
      })
    }
    return new Response(JSON.stringify({ error: 'not found' }), { status: 404, headers: { 'Content-Type': 'application/json' } })
  }))
}

function stubEmptyList() {
  vi.stubGlobal('fetch', vi.fn(async () =>
    new Response(JSON.stringify({ data: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } }),
  ))
}

function jsonResponse(body: unknown, init?: { status?: number }): Response {
  return new Response(JSON.stringify(body), { status: init?.status ?? 200, headers: { 'Content-Type': 'application/json' } })
}
function textResponse(body: string): Response {
  return new Response(body, { status: 200, headers: { 'Content-Type': 'text/plain' } })
}

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
})

describe('ArtifactPanel as a result view (#1178)', () => {
  // (a) Finished plan node: ONE tap shows the rendered plan under the
  // node's own name, with the judge timeline pinned under the header -
  // there is no second tap anywhere (no picker step).
  it("opens directly onto the node's rendered result with the judge-round timeline", async () => {
    stubPlanFixture()
    const { container } = render(<ArtifactPanel chatId="chat-1" nodeId="planner-1" nodeAgent="Planner" nodeTask="Plan the fix" nodeArtifactKind="text" onClose={() => {}} />)

    // The <h2> is the node's agent label, never an artifact id or the raw
    // task prompt (#1216).
    const heading = await screen.findByRole('heading', { level: 2, name: 'Planner' })
    expect(heading).toBeTruthy()

    // The primary output (the node's declared kind) is rendered as
    // markdown at its LATEST revision, straight away.
    expect(await screen.findByRole('heading', { level: 1, name: 'Plan v2' })).toBeTruthy()
    expect(await screen.findByText('Revision 2 of 2')).toBeTruthy()

    // Both judge rounds are chips, in round order, with the overall 0-1
    // score shown as-is.
    expect(await screen.findByRole('button', { name: 'Round 1, failed, score 0.42' })).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Round 2, passed, score 0.81' })).toBeTruthy()
    expect(screen.getByRole('group', { name: 'Judge rounds' })).toBeTruthy()

    // The sheet: h-dvh container, one scrolling region, and no native
    // <select> anywhere in the panel (the picker is gone).
    const dialog = container.querySelector('dialog')
    expect(dialog?.className).toMatch(/h-dvh/)
    expect(container.querySelectorAll('select')).toHaveLength(0)
  })

  // (#1216 review): a real agent prompt can run to a kilobyte-plus - it must
  // never render as the header, only inside Details.
  it('titles the panel with the agent label, not the raw task, however long the task is', async () => {
    const user = userEvent.setup()
    stubPlanFixture()
    const longTask = 'Investigate and fix the bug. '.repeat(70) // ~2kB
    render(<ArtifactPanel chatId="chat-1" nodeId="planner-1" nodeAgent="Planner" nodeTask={longTask} nodeArtifactKind="text" onClose={() => {}} />)

    const heading = await screen.findByRole('heading', { level: 2, name: 'Planner' })
    expect(heading.textContent).toBe('Planner')
    expect(document.body.textContent ?? '').not.toContain(longTask)

    await screen.findByRole('heading', { level: 1, name: 'Plan v2' })
    await user.click(screen.getByText('Details'))
    expect(await screen.findByText(longTask.trim())).toBeTruthy()
  })

  // (a/2) Tapping a chip: switches to the revision that round judged
  // (from the round's scored ref), stamps that round's notes as
  // highlights, and marks itself active - through the #1139 anchor
  // machinery, untouched.
  it("tapping a judge chip shows that round's notes on the revision it judged", async () => {
    const user = userEvent.setup()
    stubPlanFixture()
    render(<ArtifactPanel chatId="chat-1" nodeId="planner-1" nodeAgent="Planner" nodeTask="Plan the fix" nodeArtifactKind="text" onClose={() => {}} />)
    await screen.findByRole('heading', { level: 1, name: 'Plan v2' })

    await user.click(screen.getByRole('button', { name: 'Round 1, failed, score 0.42' }))

    // The cursor jumps to revision 1 and that revision's content renders.
    expect(await screen.findByText('Revision 1 of 2')).toBeTruthy()
    expect(screen.getByRole('heading', { level: 1, name: 'Plan v1' })).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Round 1, failed, score 0.42' }).getAttribute('aria-pressed')).toBe('true')

    // The round's note anchors onto the rendered line (snippet match) as
    // its own reachable control; clicking it opens the note readout.
    const note = await screen.findByRole('button', { name: /Judge note on line 3/ })
    await user.click(note)
    expect(await screen.findByText('Cite the source.')).toBeTruthy()
  })

  // (a/3) Manual prev/next clears the active round: notes are anchored to
  // the round's judged revision, so a manual move must not keep
  // highlighting lines the round never judged.
  it('moving between revisions clears the active round and its highlights', async () => {
    const user = userEvent.setup()
    stubPlanFixture()
    render(<ArtifactPanel chatId="chat-1" nodeId="planner-1" nodeAgent="Planner" nodeTask="Plan the fix" nodeArtifactKind="text" onClose={() => {}} />)
    await screen.findByRole('heading', { level: 1, name: 'Plan v2' })
    await user.click(screen.getByRole('button', { name: 'Round 1, failed, score 0.42' }))
    expect(await screen.findByText('Revision 1 of 2')).toBeTruthy()
    expect(await screen.findByRole('button', { name: /Judge note on line/ })).toBeTruthy()

    await user.click(screen.getByRole('button', { name: 'Next revision' }))
    expect(await screen.findByText('Revision 2 of 2')).toBeTruthy()
    expect(screen.queryByRole('button', { name: /Judge note on line/ })).toBeNull()
    expect(screen.getByRole('button', { name: 'Round 1, failed, score 0.42' }).getAttribute('aria-pressed')).toBe('false')
  })

  // (a/4) Revision bar ends: prev/next are disabled at the first and last
  // revisions; the aria-live readout carries the cursor.
  it('disables prev/next at the ends of the revision list', async () => {
    const user = userEvent.setup()
    stubPlanFixture()
    render(<ArtifactPanel chatId="chat-1" nodeId="planner-1" nodeAgent="Planner" nodeTask="Plan the fix" nodeArtifactKind="text" onClose={() => {}} />)
    await screen.findByText('Revision 2 of 2')

    // At the latest revision, Next is disabled...
    expect(screen.getByRole('button', { name: 'Next revision' })).toHaveProperty('disabled', true)
    await user.click(screen.getByRole('button', { name: 'Previous revision' }))
    await screen.findByText('Revision 1 of 2')
    // ...and at the first, Prev is.
    expect(screen.getByRole('button', { name: 'Previous revision' })).toHaveProperty('disabled', true)
  })

  // (a/5) Diff: a single toggle, always against the previous revision -
  // no "against" picker. On revision 1 it is disabled with a visible
  // reason.
  it('toggles the diff against the previous revision and explains when it is disabled', async () => {
    const user = userEvent.setup()
    stubPlanFixture()
    render(<ArtifactPanel chatId="chat-1" nodeId="planner-1" nodeAgent="Planner" nodeTask="Plan the fix" nodeArtifactKind="text" onClose={() => {}} />)
    await screen.findByText('Revision 2 of 2')

    await user.click(screen.getByRole('button', { name: 'Diff' }))
    // The unified diff of (1 -> 2) replaces the body: the added lines
    // show, the rendered heading is gone.
    expect(await screen.findByText('+# Plan v2')).toBeTruthy()
    expect(screen.queryByRole('heading', { level: 1, name: 'Plan v2' })).toBeNull()
    expect(screen.getByRole('button', { name: 'Diff' }).getAttribute('aria-pressed')).toBe('true')

    // At revision 1 there is no previous revision - disabled, with the
    // reason next to the toggle, not after a failed request.
    await user.click(screen.getByRole('button', { name: 'Previous revision' }))
    await screen.findByText('Revision 1 of 2')
    expect(screen.getByRole('button', { name: 'Diff' })).toHaveProperty('disabled', true)
    expect(screen.getByText('No previous revision')).toBeTruthy()
  })

  // (a/6) A 413 from the diff endpoint (over the 256KB bound) degrades to
  // a disabled toggle with the panel's OWN reason - the server's 413
  // message embeds the artifact id, which may only appear in Details.
  it('degrades the diff toggle to a disabled state with its own reason on a 413', async () => {
    const user = userEvent.setup()
    stubLargeFixture()
    const { container } = render(<ArtifactPanel chatId="chat-1" nodeId="writer-1" nodeAgent="Writer" nodeTask="Write the doc" onClose={() => {}} />)
    await screen.findByText('Revision 2 of 2')

    await user.click(screen.getByRole('button', { name: 'Diff' }))
    await waitFor(() => expect(screen.getByRole('button', { name: 'Diff' })).toHaveProperty('disabled', true))
    expect(screen.getByText('Too large to diff (256 KB limit)')).toBeTruthy()
    // The server's message (with the id in it) must not be rendered.
    expect(container.textContent ?? '').not.toContain('text:big')
    expect(container.textContent ?? '').not.toContain('262144')
  })

  // (a/7) More: secondary artifacts live behind labelled bottom
  // disclosures with human names and counts; each item expands INLINE
  // into the same renderer stack with its own Revision N of M prev/next -
  // no selects at any level.
  it('groups secondary artifacts under human labels and expands them inline', async () => {
    const user = userEvent.setup()
    stubPlanFixture()
    render(<ArtifactPanel chatId="chat-1" nodeId="planner-1" nodeAgent="Planner" nodeTask="Plan the fix" nodeArtifactKind="text" onClose={() => {}} />)
    await screen.findByRole('heading', { level: 1, name: 'Plan v2' })

    // Grouped, counted, human-labelled (the dispatch-authored code_review
    // reads as "Review", the finding as "Findings").
    expect(await screen.findByText('Findings (1)')).toBeTruthy()
    expect(screen.getByText('Review (1)')).toBeTruthy()

    // The finding's row is its ordinal, not its content-hash id...
    await user.click(screen.getByText('Findings (1)'))
    expect(await screen.findByRole('button', { name: '#1' })).toBeTruthy()
    // ...and it expands inline with its own revision bar and renderer.
    await user.click(screen.getByRole('button', { name: '#1' }))
    expect(await screen.findByText('Revision 1 of 1')).toBeTruthy()
    expect(await screen.findByText(/missing nil check/)).toBeTruthy()
  })

  // (b) Failed node, no artifacts: the panel says what happened - no
  // timeline, no picker, no "pick something" phrasing.
  it('shows the failure text for a failed node with no artifacts', async () => {
    stubEmptyList()
    render(<ArtifactPanel chatId="chat-1" nodeId="planner-1" nodeAgent="Planner" nodeTask="Plan the fix" nodeError="judge gave up after 3 rounds" onClose={() => {}} />)

    expect(await screen.findByText('This node failed before writing its result.')).toBeTruthy()
    expect(screen.getByText('judge gave up after 3 rounds')).toBeTruthy()
    expect(screen.queryByRole('group', { name: 'Judge rounds' })).toBeNull()
    expect(screen.queryAllByRole('button', { name: /^Round \d+/ })).toHaveLength(0)
    expect(document.querySelectorAll('select')).toHaveLength(0)
  })

  // (b/2) No artifacts, no failure: the neutral empty state.
  it("says a quiet node hasn't produced anything yet", async () => {
    stubEmptyList()
    render(<ArtifactPanel chatId="chat-1" nodeId="planner-1" nodeAgent="Planner" nodeTask="Plan the fix" onClose={() => {}} />)

    expect(await screen.findByText("This node hasn't produced anything yet.")).toBeTruthy()
    expect(screen.queryByRole('group', { name: 'Judge rounds' })).toBeNull()
  })

  // (c) No string matching an artifact id (kind + ":" + instance) appears
  // anywhere outside the collapsed Details disclosure - in BOTH themes, at
  // the narrow (390x844 sheet) and desktop (1280 card) widths. Details,
  // when opened, is the ONLY place id/kind/class/lineage/REST link appear.
  // jsdom can't size a viewport and the panel no longer reads matchMedia
  // (one tree at every width - the container class carries the
  // difference), so the stub below exists for the acceptance line, not
  // for any branch of the component.
  const ID_PATTERN = /\b(text|bytes|finding|code_review|document|pr_body|judge_round):/
  const widths: Array<[string, boolean]> = [['narrow (390x844 sheet)', true], ['desktop (1280 card)', false]]
  for (const [widthLabel, narrow] of widths) {
    for (const theme of ['light', 'dark']) {
      it(`keeps every artifact id inside Details - ${widthLabel}, ${theme}`, async () => {
        const user = userEvent.setup()
        vi.stubGlobal('matchMedia', vi.fn(() => ({ matches: narrow, media: 'x', addEventListener: () => {}, removeEventListener: () => {} })))
        stubPlanFixture()
        if (theme === 'dark') document.documentElement.classList.add('dark')
        const { container } = render(<ArtifactPanel chatId="chat-1" nodeId="planner-1" nodeAgent="Planner" nodeTask="Plan the fix" nodeArtifactKind="text" onClose={() => {}} />)
        await screen.findByRole('heading', { level: 1, name: 'Plan v2' })
        await screen.findByRole('button', { name: 'Round 2, passed, score 0.81' })

        const detailsEl = [...container.querySelectorAll('details')].find(d => d.querySelector('summary')?.textContent?.trim() === 'Details') as HTMLDetailsElement
        // Collapsed by default in both themes, and rendered lazily: a
        // closed disclosure carries no id-shaped text in the DOM at all.
        expect(detailsEl).toBeTruthy()
        expect(detailsEl.hasAttribute('open')).toBe(false)
        expect(container.textContent ?? '').not.toMatch(ID_PATTERN)

        // Opening Details is the only place the id, kind, class, lineage
        // and the REST link appear.
        await user.click(screen.getByText('Details'))
        expect(detailsEl).toHaveProperty('open', true)
        const open = container.textContent ?? ''
        expect(open).toContain('text:plan')
        expect(open).toContain('kind:text')
        expect(open).toContain('blob')
        expect(open).toContain('node:planner-1')
        expect(open).toContain('/api/v1/chats/chat-1/artifacts/text:plan?revision=2')
      })
    }
  }
})
