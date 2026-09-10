import type { Meta, StoryObj } from '@storybook/react-vite'
import { within, waitFor } from 'storybook/test'
import { ArtifactPanel } from './ArtifactPanel'
import { ChatStoreProvider } from '../state/ChatStoreProvider'
import { ChatStore } from '../state/chatStore'

const meta: Meta<typeof ArtifactPanel> = {
  title: 'Chat/ArtifactPanel',
  component: ArtifactPanel,
  parameters: { layout: 'fullscreen' },
  // The panel reads chatStore for live SSE follow (#1114) - every story
  // needs the provider, same as the real app's tree under Chat.tsx.
  decorators: [Story => <ChatStoreProvider><Story /></ChatStoreProvider>],
}
export default meta

type Story = StoryObj<typeof ArtifactPanel>

// The panel talks to the real REST client, so a story stubs global.fetch
// with canned responses matching the generated schema - no MSW in this repo
// (frontend-design skill), and this is its whole surface. Routes on the chat id in the URL: chat-1 (a finished review node), chat-failed (a failed node, no artifacts), chat-more (lots of secondary artifacts).
const findingV1 = JSON.stringify({ path: 'a.go', title: 'missing nil check', rationale: 'x may be nil here', severity: 'high' })
const findingV2 = JSON.stringify({ path: 'a.go', title: 'missing nil check (fixed)', rationale: 'x may be nil here', severity: 'high' })
// One fixed review format (internal/vetting/reviewoverview.go): the panel
// renders `rendered` (server-computed) as markdown rather than reimplement
// the renderer in TSX - codeReviewNew covers the new fields, codeReviewLegacy is pre-migration (only `summary`, no `rendered`) so the panel falls back to the generic JSON tree (LegacyCodeReview story).
const codeReviewNew = {
  verdict: 'request_changes',
  takeaway: 'Two blocking issues remain in the fallback path.',
  verified: ['Ran the auth test suite locally'],
  notes: ['Consider a changelog entry for the new endpoint'],
  finding_ids: [],
  dismissed: [],
  clean: [],
  rendered: '**Verdict: request changes** · 1 blocking\n\n' +
    'Two blocking issues remain in the fallback path.\n\n' +
    '### Verified\n\n- Ran the auth test suite locally\n\n' +
    '### Notes\n\n- Consider a changelog entry for the new endpoint',
}
const codeReviewLegacy = {
  verdict: 'approve',
  summary: 'Pre-migration free-text summary: looks fine overall, nothing blocking, a couple of minor style nits worth a follow-up but not gating the merge.',
  finding_ids: [],
  dismissed: [],
  clean: [],
}
const reviewMd = '# Review summary\n\nMostly solid, but the apple pie recipe needs a citation.\n\n- item one\n- item two\n\n```go\nfunc f() {}\n```\n'
const reviewMdV1 = '# Review draft\n\nThe apple pie recipe paragraph has no source at all.\n'
const reviewJudge = JSON.stringify({
  round: 1,
  passed: false,
  score: 0.6,
  // Judge criteria are 0-3 by design (#941), not a 0-1 fraction (#1139).
  criteria: [{ name: 'evidence', score: 1.5 }, { name: 'coverage', score: 2.5 }],
  // The round judged revision 1 of the review - tapping the chip jumps to
  // it and stamps its notes.
  scored: [{ artifact_id: 'text:review-1', revision: 1 }],
  notes: [
    { ref: { artifact_id: 'text:review-1', revision: 1, snippet: 'apple pie recipe' }, text: 'This needs a concrete source.', criterion: 'evidence' },
    { ref: { artifact_id: 'text:review-1', revision: 1, line_hint: 99 }, text: 'Unanchored: line_hint out of range in this fixture.' },
  ],
})
const reviewJudge2 = JSON.stringify({
  round: 2,
  passed: true,
  score: 0.81,
  scored: [{ artifact_id: 'text:review-1', revision: 2 }],
  notes: [],
})

// Flipped mid-story (LiveUpdate's play function) to simulate the server
// having written a 3rd revision - the fixture, not just the SSE event,
// has to reflect it since the panel's refresh is a real REST refetch.
let reviewRev3Written = false
const reviewMdV3 = '# Review summary (live update)\n\nA third revision just landed over SSE.\n'

window.fetch = async (input: RequestInfo | URL) => {
  // The generated client's per-request fetch always passes a real Request
  // instance (client.gen.ts) - String(request) is "[object Request]", so its
  // .url must be read; getArtifactText's plain fetch() still passes a bare string (the ternary covers it). buildUrl percent-encodes artifact_name (":" -> "%3A").
  const url = decodeURIComponent(input instanceof Request ? input.url : String(input))

  if (url.includes('/chats/chat-failed/')) {
    return jsonResponse({ data: [] }) // nothing for a failed node
  }
  if (url.includes('/chats/chat-review-legacy/')) {
    if (url.endsWith('/artifacts')) {
      return jsonResponse({
        data: [{ name: 'code_review:pr:legacy', kind: 'code_review', class: 'structured', latest_revision: 1, lineage: { node_id: 'reviewer-1', author: 'gate' }, revisions: [] }],
      })
    }
    if (url.includes('/artifacts/code_review:pr:legacy')) {
      if (url.includes('/revisions')) return jsonResponse({ data: [{ revision: 1, mime_type: 'application/json', size: 10, kind: 'code_review', class: 'structured', lineage: { node_id: 'reviewer-1', author: 'gate' } }] })
      return textResponse(JSON.stringify(codeReviewLegacy))
    }
    return jsonResponse({ data: [] })
  }
  if (url.includes('/chats/chat-more/')) {
    if (url.endsWith('/artifacts')) {
      return jsonResponse({
        data: [
          { name: 'document:spec', kind: 'document', class: 'structured', latest_revision: 1, lineage: { node_id: 'writer-1', author: 'worker' }, revisions: [] },
          { name: 'finding:692b00ee', kind: 'finding', class: 'structured', latest_revision: 1, lineage: { node_id: 'writer-1', author: 'worker' }, revisions: [] },
          { name: 'finding:0f4c1a22', kind: 'finding', class: 'structured', latest_revision: 1, lineage: { node_id: 'writer-1', author: 'worker' }, revisions: [] },
          { name: 'finding:77aa39be', kind: 'finding', class: 'structured', latest_revision: 1, lineage: { node_id: 'writer-1', author: 'worker' }, revisions: [] },
          { name: 'code_review:pr:1', kind: 'code_review', class: 'structured', latest_revision: 1, lineage: { node_id: 'writer-1', author: 'dispatch' }, revisions: [] },
          { name: 'pr_body:1', kind: 'pr_body', class: 'blob', latest_revision: 1, lineage: { node_id: 'writer-1', author: 'worker' }, revisions: [] },
          { name: 'bytes:logo', kind: 'bytes', class: 'blob', latest_revision: 1, lineage: { node_id: 'writer-1', author: 'worker' }, revisions: [] },
          { name: 'text:notes', kind: 'text', class: 'blob', latest_revision: 1, lineage: { node_id: 'writer-1', author: 'worker' }, revisions: [] },
        ],
      })
    }
    if (url.includes('/artifacts/document:spec')) {
      if (url.includes('/revisions')) return jsonResponse({ data: [{ revision: 1, mime_type: 'application/json', size: 20, kind: 'document', class: 'structured', lineage: { node_id: 'writer-1', author: 'worker' } }] })
      return textResponse(JSON.stringify({ title: 'The spec', body: 'Spec body.' }))
    }
    const findingNames = ['finding:692b00ee', 'finding:0f4c1a22', 'finding:77aa39be']
    if (findingNames.some(f => url.includes(`/artifacts/${f}`))) {
      const f = findingNames.find(x => url.includes(`/artifacts/${x}`))!
      if (url.includes('/revisions')) return jsonResponse({ data: [{ revision: 1, mime_type: 'application/json', size: 10, kind: 'finding', class: 'structured', lineage: { node_id: 'writer-1', author: 'worker' } }] })
      return textResponse(JSON.stringify({ path: 'a.go', title: `finding ${f.slice(-4)}`, rationale: 'demo' }))
    }
    if (url.includes('/artifacts/code_review:pr:1')) {
      if (url.includes('/revisions')) return jsonResponse({ data: [{ revision: 1, mime_type: 'application/json', size: 10, kind: 'code_review', class: 'structured', lineage: { node_id: 'writer-1', author: 'dispatch' } }] })
      return textResponse(JSON.stringify(codeReviewNew))
    }
    if (url.includes('/artifacts/code_review:pr:legacy')) {
      if (url.includes('/revisions')) return jsonResponse({ data: [{ revision: 1, mime_type: 'application/json', size: 10, kind: 'code_review', class: 'structured', lineage: { node_id: 'reviewer-1', author: 'gate' } }] })
      return textResponse(JSON.stringify(codeReviewLegacy))
    }
    if (url.includes('/artifacts/pr_body:1')) {
      if (url.includes('/revisions')) return jsonResponse({ data: [{ revision: 1, mime_type: 'text/markdown', size: 10, kind: 'pr_body', class: 'blob', lineage: { node_id: 'writer-1', author: 'worker' } }] })
      return textResponse('# PR description\n\nWhat and why, briefly.')
    }
    if (url.includes('/artifacts/bytes:logo')) {
      if (url.includes('/revisions')) return jsonResponse({ data: [{ revision: 1, mime_type: 'image/png', size: 10, kind: 'bytes', class: 'blob', lineage: { node_id: 'writer-1', author: 'worker' } }] })
      return textResponse('<binary png>')
    }
    if (url.includes('/artifacts/text:notes')) {
      if (url.includes('/revisions')) return jsonResponse({ data: [{ revision: 1, mime_type: 'text/markdown', size: 10, kind: 'text', class: 'blob', lineage: { node_id: 'writer-1', author: 'worker' } }] })
      return textResponse('# Notes\n\nWorking notes here.')
    }
    return jsonResponse({ data: [] })
  }

  // chat-1: the finished review node (#1178's primary story).
  if (url.includes('/artifacts/text:review-1/revisions')) {
    return jsonResponse({
      data: [
        // Newest first, like the endpoint (openapi.yaml's ArtifactRevisionList).
        ...(reviewRev3Written ? [{ revision: 3, mime_type: 'text/markdown', size: reviewMdV3.length, kind: 'text', class: 'blob', lineage: { node_id: 'reviewer-1', round: 3, author: 'worker' } }] : []),
        { revision: 2, mime_type: 'text/markdown', size: reviewMd.length, kind: 'text', class: 'blob', lineage: { node_id: 'reviewer-1', round: 2, author: 'worker', trigger_annotation: 'judge_round:t1-1-1' } },
        { revision: 1, mime_type: 'text/markdown', size: reviewMdV1.length, kind: 'text', class: 'blob', lineage: { node_id: 'reviewer-1', round: 1, author: 'worker' } },
      ],
    })
  }
  if (url.includes('/artifacts/finding:abc123/revisions')) {
    return jsonResponse({
      data: [
        { revision: 2, mime_type: 'application/json', size: findingV2.length, kind: 'finding', class: 'structured', lineage: { node_id: 'reviewer-1', round: 1, author: 'worker' } },
        { revision: 1, mime_type: 'application/json', size: findingV1.length, kind: 'finding', class: 'structured', lineage: { node_id: 'reviewer-1', round: 1, author: 'worker' } },
      ],
    })
  }
  if (url.includes('/artifacts/text:review-1?revision=3')) return textResponse(reviewMdV3)
  if (url.includes('/artifacts/text:review-1?revision=2')) return textResponse(reviewMd)
  if (url.includes('/artifacts/text:review-1?revision=1')) return textResponse(reviewMdV1)
  if (url.includes('/artifacts/finding:abc123?revision=2')) return textResponse(findingV2)
  if (url.includes('/artifacts/finding:abc123?revision=1')) return textResponse(findingV1)
  if (url.includes('/artifacts/text:review-1/diff')) return textResponse(`--- text:review-1@1\n+++ text:review-1@2\n@@ -1,2 +1,5 @@\n-# Review draft\n-Old line.\n+# Review summary\n+New lines here.\n`)
  if (url.includes('/artifacts/judge_round:t1-1-1')) return textResponse(reviewJudge)
  if (url.includes('/artifacts/judge_round:t1-1-2')) return textResponse(reviewJudge2)
  if (url.endsWith('/artifacts')) {
    return jsonResponse({
      data: [
        { name: 'text:review-1', kind: 'text', class: 'blob', latest_revision: 2, lineage: { node_id: 'reviewer-1', round: 2, author: 'worker' }, revisions: [] },
        { name: 'finding:abc123', kind: 'finding', class: 'structured', latest_revision: 2, lineage: { node_id: 'reviewer-1', round: 1, author: 'worker' }, revisions: [] },
        { name: 'code_review:pr:1', kind: 'code_review', class: 'structured', latest_revision: 1, lineage: { node_id: 'reviewer-1', round: 1, author: 'dispatch' }, revisions: [] },
        { name: 'judge_round:t1-1-1', kind: 'judge_round', class: 'structured', latest_revision: 1, lineage: { node_id: 'reviewer-1', author: 'judge' }, revisions: [] },
        { name: 'judge_round:t1-1-2', kind: 'judge_round', class: 'structured', latest_revision: 1, lineage: { node_id: 'reviewer-1', author: 'judge' }, revisions: [] },
      ],
    })
  }
  return jsonResponse({ data: [] })
}

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
}
function textResponse(body: string): Response {
  return new Response(body, { status: 200, headers: { 'Content-Type': 'text/plain' } })
}

// A finished review node (#1178): opens on the node's result (the review
// markdown under the node's own name) with the two-round judge timeline
// under the header, "More" for finding/code_review, provenance in Details. Verified light; WithResultDark pins dark - #1114's "black on gray" complaint was found in this exact panel.
export const WithResult: Story = {
  args: {
    chatId: 'chat-1',
    nodeId: 'reviewer-1',
    nodeAgent: 'Code Reviewer',
    nodeTask: 'Review PR #1170',
    nodeArtifactKind: 'text',
    onClose: () => {},
  },
}

// Minimal EventSource fake (mirrors chatStore.test.ts's own) so the play
// function can dispatch a real SSE frame through chatStore's own
// EventSource handling, rather than reaching into the store's internals.
class FakeEventSource {
  static last: FakeEventSource | null = null
  onerror: (() => void) | null = null
  private listeners: Record<string, ((e: MessageEvent) => void)[]> = {}
  constructor() { FakeEventSource.last = this }
  addEventListener(name: string, cb: (e: MessageEvent) => void) { (this.listeners[name] ??= []).push(cb) }
  close() {}
  emit(name: string, data: unknown) {
    for (const cb of this.listeners[name] ?? []) cb({ data: JSON.stringify(data), lastEventId: '' } as MessageEvent)
  }
}
window.EventSource = FakeEventSource as unknown as typeof EventSource

// #1114: the panel follows a live artifact_revision event over the chat's
// own SSE stream, no page reload - the story injects its own ChatStore,
// attaches it (opening the fake EventSource above), and play fires the event exactly as the real stream would.
const liveStore = new ChatStore()
liveStore.seed('chat-1', [])
liveStore.attach('chat-1')
export const LiveUpdate: Story = {
  ...WithResult,
  decorators: [Story => <ChatStoreProvider store={liveStore}><Story /></ChatStoreProvider>],
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await waitFor(() => canvas.getByText('Revision 2 of 2'))

    reviewRev3Written = true
    FakeEventSource.last?.emit('artifact_revision', { id: 'text:review-1', revision: 3, kind: 'text', node_id: 'reviewer-1', round: 3 })

    await waitFor(() => canvas.getByText('Revision 3 of 3'))
  },
}

// The bug this story exists to catch (#1216 review): a real agent prompt can
// run to a kilobyte-plus. The header must stay 2 lines regardless - the full
// text is only reachable in Details.
export const WithLongTask: Story = {
  ...WithResult,
  args: {
    ...WithResult.args,
    nodeTask: 'Review PR #1170 for correctness, style, and security issues. '.repeat(40),
  },
}

export const WithResultDark: Story = {
  ...WithResult,
  // A local `.dark` ancestor is enough: Tailwind's dark: variant matches any
  // dark-classed ancestor, not just <html>, so this pins the theme without
  // touching the global toolbar toggle .storybook/preview.tsx provides.
  decorators: [Story => <div className="dark"><Story /></div>],
}

// #1178 mobile: the dialog is a full-height bottom sheet (h-dvh) below
// `sm`, the timeline pinned under the header. No viewport addon in this repo
// (.storybook/main.ts's addons list is empty), so this wraps the fixed 390x844 box directly - a real, sized container the panel's own `sm:` breakpoint reacts to exactly like a real small screen (DevTools emulation and this box agree at 390px).
export const WithResultMobile: Story = {
  ...WithResult,
  decorators: [Story => (
    <div style={{ width: 390, height: 844, border: '1px solid #888', overflow: 'hidden' }}>
      <Story />
    </div>
  )],
}

// A failed node: no artifacts at all. The panel says what happened instead
// of offering anything to pick.
export const FailedNode: Story = {
  args: {
    chatId: 'chat-failed',
    nodeId: 'reviewer-1',
    nodeAgent: 'Code Reviewer',
    nodeTask: 'Review PR #1170',
    nodeError: 'judge gave up after 3 rounds without a passing score',
    onClose: () => {},
  },
}

// A node with lots of secondary artifacts: the "More" section's labelled
// groups (Findings, Review, PR description, Document, Files) each expand
// inline with their own Revision N of M prev/next - no selects at any level.
export const MoreHeavyNode: Story = {
  args: {
    chatId: 'chat-more',
    nodeId: 'writer-1',
    nodeAgent: 'Writer',
    nodeTask: 'Write the spec',
    nodeArtifactKind: 'document',
    onClose: () => {},
  },
}

// A pre-migration code_review record: `summary` but no `rendered` field at
// all (old history, never backfilled), so the panel falls back to the
// generic JSON tree; the codeReviewNew fixture covers the new shape.
export const LegacyCodeReview: Story = {
  args: {
    chatId: 'chat-review-legacy',
    nodeId: 'reviewer-1',
    nodeAgent: 'Code Reviewer',
    nodeTask: 'Review PR #1170',
    nodeArtifactKind: 'code_review',
    onClose: () => {},
  },
}
