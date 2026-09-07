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

// The panel talks to the real REST client (frontend/src/api.ts), so a story
// stubs global.fetch with canned responses matching the generated schema -
// no MSW in this repo (see frontend-design skill: reach for native/existing
// before a new dependency), and this is the whole surface it calls. The
// stub routes on the chat id in the URL so each story gets its own fixture:
// chat-1 (a finished review node), chat-failed (a failed node, no
// artifacts), chat-more (a node with lots of secondary artifacts).
const findingV1 = JSON.stringify({ path: 'a.go', title: 'missing nil check', rationale: 'x may be nil here', severity: 'high' })
const findingV2 = JSON.stringify({ path: 'a.go', title: 'missing nil check (fixed)', rationale: 'x may be nil here', severity: 'high' })
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
  // The generated client's per-request fetch always calls this with a real
  // Request instance (client.gen.ts) - String(aRequest) is "[object
  // Request]", so its own .url must be read; getArtifactText's plain
  // fetch() call still passes a bare string, which the ternary also covers.
  // buildUrl percent-encodes the artifact_name path param (":" -> "%3A").
  const url = decodeURIComponent(input instanceof Request ? input.url : String(input))

  if (url.includes('/chats/chat-failed/')) {
    return jsonResponse({ data: [] }) // nothing for a failed node
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
      return textResponse(JSON.stringify({ verdict: 'approve', comments: [] }))
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

// A finished review node (#1178): the panel opens directly on the node's
// result - the review markdown under the node's own name - with the
// two-round judge timeline pinned under the header. Tap a chip to see that
// round's notes on the revision it judged; the revision bar diffs against
// the previous revision; the finding and the dispatch-authored
// code_review sit behind "More"; id/kind/class/lineage/REST link sit in
// the collapsed "Details" at the bottom. Verified against the global theme
// toolbar (light, the Storybook default); WithResultDark below is the same
// fixture pinned to dark so both are checkable without touching the
// toolbar - #1114 owner feedback ("dark mode looks awful, text is black on
// gray") was found in this exact panel.
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
// own SSE stream, no page reload/manual Refresh - the story injects its own
// ChatStore, attaches it (opening the fake EventSource above), and the play
// function fires the event exactly as the real stream would deliver it.
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
  // A local `.dark` ancestor is enough - Tailwind's dark: custom-variant
  // (`&:where(.dark, .dark *)`) matches any dark-classed ancestor, not just
  // <html>, so this pins the theme without touching the global toolbar
  // toggle .storybook/preview.tsx already provides for every other story.
  decorators: [Story => <div className="dark"><Story /></div>],
}

// #1178 mobile: the dialog is a full-height bottom sheet (h-dvh) below
// `sm`, the timeline pinned under the header. No viewport addon in this
// repo (.storybook/main.ts's addons list is empty) - a tiny new dependency
// wasn't clearly worth it for one story, so this wraps the fixed 390x844
// box directly (a real, sized container the panel's own `sm:` breakpoint
// reacts to exactly like a real small screen would; Chrome DevTools device
// emulation and this box agree at 390px either way).
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
