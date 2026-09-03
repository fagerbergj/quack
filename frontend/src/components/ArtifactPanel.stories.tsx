import type { Meta, StoryObj } from '@storybook/react-vite'
import { ArtifactPanel } from './ArtifactPanel'

const meta: Meta<typeof ArtifactPanel> = {
  title: 'Chat/ArtifactPanel',
  component: ArtifactPanel,
  parameters: { layout: 'fullscreen' },
}
export default meta

type Story = StoryObj<typeof ArtifactPanel>

// The panel talks to the real REST client (frontend/src/api.ts), so a story
// stubs global.fetch with canned responses matching the generated schema -
// no MSW in this repo (see frontend-design skill: reach for native/existing
// before a new dependency), and this is the whole surface it calls.
function stubFetch() {
  const findingV1 = JSON.stringify({ path: 'a.go', title: 'missing nil check', rationale: 'x may be nil here', severity: 'high' })
  const findingV2 = JSON.stringify({ path: 'a.go', title: 'missing nil check (fixed)', rationale: 'x may be nil here', severity: 'high' })
  const reviewMd = '# Review summary\n\nMostly solid, but the apple pie recipe needs a citation.\n\n- item one\n- item two\n\n```go\nfunc f() {}\n```\n'
  const judgeRound = JSON.stringify({
    round: 1,
    passed: false,
    score: 0.6,
    criteria: [{ name: 'evidence', score: 0.4 }, { name: 'coverage', score: 0.8 }],
    notes: [
      { ref: { artifact_id: 'finding:abc123', revision: 1, snippet: 'x may be nil here' }, text: 'This rationale needs a concrete repro.', criterion: 'evidence' },
      { ref: { artifact_id: 'finding:abc123', revision: 1, line_hint: 99 }, text: 'Unanchored: line_hint out of range in this fixture.' },
      { ref: { artifact_id: 'text:review-1', revision: 1, snippet: 'apple pie recipe' }, text: 'Cite the source.', criterion: 'evidence' },
    ],
  })

  window.fetch = async (input: RequestInfo | URL) => {
    // The generated client's per-request fetch always calls this with a real
    // Request instance (client.gen.ts) - String(aRequest) is "[object
    // Request]", so its own .url must be read; getArtifactText's plain
    // fetch() call still passes a bare string, which the ternary also covers.
    // buildUrl percent-encodes the artifact_name path param (":" -> "%3A").
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
      return jsonResponse({ data: [{ revision: 1, mime_type: 'application/json', size: judgeRound.length, kind: 'judge_round', class: 'structured', lineage: { node_id: 'reviewer-1', round: 1, author: 'judge' } }] })
    }
    if (url.includes('/artifacts/text:review-1/revisions')) {
      return jsonResponse({ data: [{ revision: 1, mime_type: 'text/markdown', size: reviewMd.length, kind: 'text', class: 'blob', lineage: { node_id: 'reviewer-1', round: 1, author: 'worker' } }] })
    }
    if (url.includes('/artifacts/finding:abc123?revision=2')) return textResponse(findingV2)
    if (url.includes('/artifacts/finding:abc123?revision=1')) return textResponse(findingV1)
    if (url.includes('/artifacts/finding:abc123/diff')) return textResponse(`--- finding:abc123@1\n+++ finding:abc123@2\n@@ -1 +1 @@\n-${findingV1}\n+${findingV2}\n`)
    if (url.includes('/artifacts/judge_round:t1-1')) return textResponse(judgeRound)
    if (url.includes('/artifacts/text:review-1?revision=1')) return textResponse(reviewMd)
    if (url.endsWith('/artifacts')) {
      return jsonResponse({
        data: [
          { name: 'finding:abc123', kind: 'finding', class: 'structured', latest_revision: 2, lineage: { node_id: 'reviewer-1', round: 1, author: 'worker' }, revisions: [] },
          { name: 'judge_round:t1-1', kind: 'judge_round', class: 'structured', latest_revision: 1, lineage: { node_id: 'reviewer-1', round: 1, author: 'judge' }, revisions: [] },
          { name: 'text:review-1', kind: 'text', class: 'blob', latest_revision: 1, lineage: { node_id: 'reviewer-1', round: 1, author: 'worker' }, revisions: [] },
          { name: 'code_review:pr:1', kind: 'code_review', class: 'structured', latest_revision: 1, lineage: { node_id: 'reviewer-1', round: 1, author: 'dispatch' }, revisions: [] },
        ],
      })
    }
    return jsonResponse({ data: [] })
  }
}

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
}
function textResponse(body: string): Response {
  return new Response(body, { status: 200, headers: { 'Content-Type': 'text/plain' } })
}

stubFetch()

// A reviewer node's one finding (JSON tree by default - #1114), a markdown
// review blob (rendered headings/lists/code, not raw text), and a
// judge_round (criteria chips + collapsible notes array) - one judge note
// anchors to the finding's rationale line, one to the markdown's own quoted
// line, plus an intentionally out-of-range line_hint for the unanchored list.
// Verified against the global theme toolbar (light, the Storybook default);
// WithJudgeNotesDark below is the same fixture pinned to dark so both are
// checkable without touching the toolbar - #1114 owner feedback ("dark mode
// looks awful, text is black on gray") was found in this exact panel.
export const WithJudgeNotes: Story = {
  args: {
    chatId: 'chat-1',
    nodeId: 'reviewer-1',
    onClose: () => {},
  },
}

export const WithJudgeNotesDark: Story = {
  ...WithJudgeNotes,
  // A local `.dark` ancestor is enough - Tailwind's dark: custom-variant
  // (`&:where(.dark, .dark *)`) matches any dark-classed ancestor, not just
  // <html>, so this pins the theme without touching the global toolbar
  // toggle .storybook/preview.tsx already provides for every other story.
  decorators: [Story => <div className="dark"><Story /></div>],
}

// #1114 mobile pass: no viewport addon in this repo (.storybook/main.ts's
// addons list is empty) - a tiny new dependency wasn't clearly worth it for
// one story, so this wraps the fixed 360x740 box directly (a real, sized
// container the panel's own `sm:` breakpoint reacts to exactly like a real
// small screen would - `window.matchMedia` used by useIsNarrow reads the
// VIEWPORT, not this box, but Chrome DevTools device emulation and this
// box agree at 360px either way).
export const WithJudgeNotesMobile: Story = {
  ...WithJudgeNotes,
  decorators: [Story => (
    <div style={{ width: 360, height: 740, border: '1px solid #888', overflow: 'hidden' }}>
      <Story />
    </div>
  )],
}
