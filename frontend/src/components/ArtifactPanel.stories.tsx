import type { Meta, StoryObj } from '@storybook/react-vite'
import { within, waitFor } from 'storybook/test'
import { ArtifactPanel } from './ArtifactPanel'
import { ChatStoreProvider } from '../state/ChatStoreProvider'
import { ChatStore } from '../state/chatStore'

const meta: Meta<typeof ArtifactPanel> = {
  title: 'Chat/ArtifactPanel',
  component: ArtifactPanel,
  // Every story renders at 390px too: the dialog is a bottom sheet below
  // `medium`, and a typed view (table, snippet block) is exactly what overflows a narrow width first.
  parameters: { layout: 'fullscreen', renderCheck: { viewports: ['mobile', 'desktop'] } },
  // The panel reads chatStore for live SSE follow (#1114) - every story
  // needs the provider, same as the real app's tree under Chat.tsx.
  decorators: [Story => <ChatStoreProvider><Story /></ChatStoreProvider>],
}
export default meta

type Story = StoryObj<typeof ArtifactPanel>

const findingV1 = JSON.stringify({ path: 'a.go', title: 'missing nil check', rationale: 'x may be nil here', severity: 'high' })
const findingV2 = JSON.stringify({ path: 'a.go', title: 'missing nil check (fixed)', rationale: 'x may be nil here', severity: 'high' })
// codeReviewNew covers every ReviewView field; codeReviewSparse (verdict
// only) covers a minimal record rendering without blanks.
const codeReviewNew = {
  verdict: 'request_changes',
  takeaway: 'Two blocking issues remain in the fallback path.',
  verified: ['Ran the auth test suite locally'],
  notes: ['Consider a changelog entry for the new endpoint'],
  finding_ids: [],
}
const codeReviewSparse = { verdict: 'approve' }
// Pre-migration shape (records written before takeaway/rendered existed) -
// only `summary`, which ReviewView must fall back to.
const codeReviewLegacy = {
  verdict: 'approve',
  summary: 'Looks fine overall, nothing blocking, a couple of minor style nits worth a follow-up.',
  finding_ids: [],
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

// The generated client's per-request fetch always passes a real Request
// instance (client.gen.ts) - String(request) is "[object Request]", so its
// .url must be read; getArtifactText's plain fetch() still passes a bare string (the ternary covers it). buildUrl percent-encodes artifact_name (":" -> "%3A").
function urlOf(input: RequestInfo | URL): string {
  return decodeURIComponent(input instanceof Request ? input.url : String(input))
}

// One artifact's two canned routes: its revision list, or its content body.
function artifactRoute(url: string, name: string, revisions: Response, body: Response): Response | null {
  if (!url.includes(`/artifacts/${name}`)) return null
  return url.includes('/revisions') ? revisions : body
}

function chatFailedRoute(url: string): Response | null {
  if (url.includes('/chats/chat-failed/')) return jsonResponse({ data: [] }) // nothing for a failed node
  return null
}

function chatReviewSparseRoute(url: string): Response | null {
  if (!url.includes('/chats/chat-review-sparse/')) return null
  if (url.endsWith('/artifacts')) {
    return jsonResponse({
      data: [{ name: 'code_review:pr:sparse', kind: 'code_review', class: 'structured', latest_revision: 1, lineage: { node_id: 'reviewer-1', author: 'gate' }, revisions: [] }],
    })
  }
  if (url.includes('/artifacts/code_review:pr:sparse')) {
    if (url.includes('/revisions')) return jsonResponse({ data: [{ revision: 1, mime_type: 'application/json', size: 10, kind: 'code_review', class: 'structured', lineage: { node_id: 'reviewer-1', author: 'gate' } }] })
    return textResponse(JSON.stringify(codeReviewSparse))
  }
  return jsonResponse({ data: [] })
}

// A node with no artifact at all, but two judge rounds - the rounds must
// stay reachable (as secondary rows) rather than becoming dead chips.
function chatJudgeOnlyRoute(url: string): Response | null {
  if (!url.includes('/chats/chat-judge-only/')) return null
  if (url.includes('/artifacts/judge_round:t1-judge-only-1')) return textResponse(reviewJudge)
  if (url.includes('/artifacts/judge_round:t1-judge-only-2')) return textResponse(reviewJudge2)
  if (url.endsWith('/artifacts')) {
    return jsonResponse({
      data: [
        { name: 'judge_round:t1-judge-only-1', kind: 'judge_round', class: 'structured', latest_revision: 1, lineage: { node_id: 'writer-1', author: 'judge' }, revisions: [] },
        { name: 'judge_round:t1-judge-only-2', kind: 'judge_round', class: 'structured', latest_revision: 1, lineage: { node_id: 'writer-1', author: 'judge' }, revisions: [] },
      ],
    })
  }
  return jsonResponse({ data: [] })
}

function chatReviewLegacyRoute(url: string): Response | null {
  if (!url.includes('/chats/chat-review-legacy/')) return null
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

// The panel talks to the real REST client, so a story stubs global.fetch
// with canned responses matching the generated schema - no MSW in this repo
// (frontend-design skill), and this is its whole surface. Routes on the chat id in the URL: chat-1 (a finished review node), chat-failed (a failed node, no artifacts), chat-more (lots of secondary artifacts).
function chatMoreRoute(url: string): Response | null {
  if (!url.includes('/chats/chat-more/')) return null
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
  const findingNames = ['finding:692b00ee', 'finding:0f4c1a22', 'finding:77aa39be']
  const finding = findingNames.find(f => url.includes(`/artifacts/${f}`))
  const findingRevisions = () => jsonResponse({ data: [{ revision: 1, mime_type: 'application/json', size: 10, kind: 'finding', class: 'structured', lineage: { node_id: 'writer-1', author: 'worker' } }] })
  return (
    artifactRoute(url, 'document:spec', jsonResponse({ data: [{ revision: 1, mime_type: 'application/json', size: 20, kind: 'document', class: 'structured', lineage: { node_id: 'writer-1', author: 'worker' } }] }), textResponse(JSON.stringify({ title: 'The spec', body: 'Spec body.' }))) ??
    (finding != null ? artifactRoute(url, finding, findingRevisions(), textResponse(JSON.stringify({ path: 'a.go', title: `finding ${finding.slice(-4)}`, rationale: 'demo' }))) : null) ??
    artifactRoute(url, 'code_review:pr:1', jsonResponse({ data: [{ revision: 1, mime_type: 'application/json', size: 10, kind: 'code_review', class: 'structured', lineage: { node_id: 'writer-1', author: 'dispatch' } }] }), textResponse(JSON.stringify(codeReviewNew))) ??
    artifactRoute(url, 'code_review:pr:sparse', jsonResponse({ data: [{ revision: 1, mime_type: 'application/json', size: 10, kind: 'code_review', class: 'structured', lineage: { node_id: 'reviewer-1', author: 'gate' } }] }), textResponse(JSON.stringify(codeReviewSparse))) ??
    artifactRoute(url, 'pr_body:1', jsonResponse({ data: [{ revision: 1, mime_type: 'text/markdown', size: 10, kind: 'pr_body', class: 'blob', lineage: { node_id: 'writer-1', author: 'worker' } }] }), textResponse('# PR description\n\nWhat and why, briefly.')) ??
    artifactRoute(url, 'bytes:logo', jsonResponse({ data: [{ revision: 1, mime_type: 'image/png', size: 10, kind: 'bytes', class: 'blob', lineage: { node_id: 'writer-1', author: 'worker' } }] }), textResponse('<binary png>')) ??
    artifactRoute(url, 'text:notes', jsonResponse({ data: [{ revision: 1, mime_type: 'text/markdown', size: 10, kind: 'text', class: 'blob', lineage: { node_id: 'writer-1', author: 'worker' } }] }), textResponse('# Notes\n\nWorking notes here.')) ??
    null
  )
}

function chatOneRoute(url: string): Response | null {
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
        { name: 'judge_round:t1-1-1', kind: 'judge_round', class: 'structured', latest_revision: 1, lineage: { node_id: 'reviewer-1', author: 'judge' }, revisions: [] },
        { name: 'judge_round:t1-1-2', kind: 'judge_round', class: 'structured', latest_revision: 1, lineage: { node_id: 'reviewer-1', author: 'judge' }, revisions: [] },
      ],
    })
  }
  return null
}

// Real shapes captured off a code-reviewer node reviewing PR #1464, long
// strings shortened; finding_ids adapted to the four findings captured here.
const reviewFindingIds = ['finding:89f3e6d1', 'finding:e11c2106', 'finding:c1a68ddf', 'finding:76f9df59']
const codeReview1464 = {
  verdict: 'approve',
  takeaway: 'Prompt facts and the re-aimed vet match the code they describe; findings are a bwrap-dependent golden test, the sweep purge\'s unreachable singletons, and minor duplication - none blocking.',
  verified: [
    'Ran go test on acp/memory/agent/vetting: memory, agent, vetting green; acp git golden failed locally (environment, see notes)',
    'Traced both sweep passes (burstClusters, cosineClusters) - only size>=2 clusters reach the model',
    'CI: 7 checks passing, 4 pending (go-test, render-check, docker-build, go-slop) at review time',
  ],
  notes: ['internal/workspace/sandbox.go (+10, GoModCachePreseeded) is omitted from the PR description but coherent.'],
  finding_ids: reviewFindingIds,
  rendered: '**Verdict: approve** · 3 suggestions · 1 nit\n\n' +
    'Prompt facts and the re-aimed vet match the code they describe; findings are a bwrap-dependent golden test, the sweep purge\'s unreachable singletons, and minor duplication - none blocking.\n\n' +
    '### Highlights\n\n| Severity | Where | Why it matters |\n| --- | --- | --- |\n' +
    '| suggestion | internal/memory/commit.go:548 | Only clusters of >=2 are returned |\n\n' +
    '### Dismissed\n\nNone.',
}
const finding89f3e6d1 = {
  line_hint: 86, path: 'internal/acp/environment_golden_test.go', severity: 'suggestion', state: 'new',
  title: 'New git golden fixture hard-depends on a working bwrap and fails instead of skipping where userns is unavailable',
  rationale: 'gitInfo runs workspace.RunArgv under SandboxBwrap; where bwrap cannot create a namespace, RunArgv returns non-zero and the golden test fails with a confusing diff instead of skipping.',
  snippet: 'sandboxed := workspace.Caps{Sandbox: workspace.SandboxBwrap, ...}',
}
const findingE11c2106 = {
  line_hint: 28, path: 'internal/acp/environment.go', severity: 'suggestion', state: 'new',
  title: 'Sandboxed computation re-states workspace.EnforcesBoundary',
  rationale: 'Identical predicate to workspace.EnforcesBoundary (sandbox.go:814). Using the helper keeps the environment block in sync if the set of boundary-enforcing modes changes.',
  snippet: 'Sandboxed: caps.Sandbox == workspace.SandboxBwrap || caps.Sandbox == workspace.SandboxLandlock,',
}
const findingC1a68ddf = {
  path: 'internal/memory/commit.go', line_hint: 548, severity: 'suggestion', state: 'resolved',
  title: 'Dedupe sweep\'s runtime-fact DELETE rule is unreachable for singleton memories',
  rationale: 'Both sweep passes feed the model only clusters of size >= 2, so the nightly purge covers only clustered memories.',
  snippet: '"runtime fact, moved to environment prompt", even with no duplicate in this burst.',
}
const finding76f9df59 = {
  path: 'internal/acp/environment_golden_test.go', line_hint: 84, severity: 'nit', state: 'resolved',
  title: 'Comment cites childEnv\'s GOMODCACHE farming, which environmentBlock never does',
  rationale: 'environmentBlock performs no mod-cache farming and does not read HomeDir for this fixture, so the stated rationale points at a code path the test never exercises.',
  snippet: '// HomeDir distinct from repo: childEnv farms a writable GOMODCACHE under HOME, which would',
}
const judgeRound1464 = {
  turn: 'e-f957a075-1', round: 1, passed: false, score: 0.3333333333333333,
  criteria: [
    { name: 'catches_real_issues', score: 1, feedback: 'The review surfaces the substantive issues and does not gate the merge on partially-pending CI.' },
    { name: 'constructive_actionable', score: 0.3333333333333333, feedback: 'Two findings propose a code change in prose without a fenced block.' },
    { name: 'severity_grounded', score: 1, feedback: 'The bwrap finding is correctly a suggestion, not a defect.' },
  ],
  evidence: { probes: [{ name: 'behaviour_verified', result: 'pass' }, { name: 'review_posted', result: 'pass' }] },
}
const dagNode1464 = { node_id: 'code-reviewer-1', agent: 'code-reviewer', status: 'done', context_id: 'pi-0t94bitnsxhp', started: true }
const dagPlan1464 = {
  plan_id: '2ce25073', status: 'done',
  assignments: [{ node_id: 'code-reviewer-1', task: 'Review PR #1464 (quack repo, fagerbergj/quack): memory: move runtime facts to environment prompt, re-aim vet.' }],
}

// CodeReviewerAllKinds and OrchestratorNode share one chat: dag_node and
// dag_plan use their real lineage (code-reviewer-1, orchestrator), so each panel opens on its own deliverable only.
function chatReviewer1466Route(url: string): Response | null {
  if (!url.includes('/chats/chat-reviewer-1466/')) return null
  if (url.endsWith('/artifacts')) {
    return jsonResponse({
      data: [
        { name: 'code_review:pr:1464', kind: 'code_review', class: 'structured', latest_revision: 2, lineage: { node_id: 'code-reviewer-1', author: 'gate' }, revisions: [] },
        { name: 'finding:89f3e6d1', kind: 'finding', class: 'structured', latest_revision: 2, lineage: { node_id: 'code-reviewer-1', author: 'worker' }, revisions: [] },
        { name: 'finding:e11c2106', kind: 'finding', class: 'structured', latest_revision: 2, lineage: { node_id: 'code-reviewer-1', author: 'worker' }, revisions: [] },
        { name: 'finding:c1a68ddf', kind: 'finding', class: 'structured', latest_revision: 2, lineage: { node_id: 'code-reviewer-1', author: 'worker' }, revisions: [] },
        { name: 'finding:76f9df59', kind: 'finding', class: 'structured', latest_revision: 2, lineage: { node_id: 'code-reviewer-1', author: 'worker' }, revisions: [] },
        { name: 'judge_round:e-f957a075-1', kind: 'judge_round', class: 'structured', latest_revision: 1, lineage: { node_id: 'code-reviewer-1', author: 'judge' }, revisions: [] },
        // dag_node is bookkeeping, never selectable; dag_plan belongs to the orchestrator, so it never appears here either.
        { name: 'dag_node:code-reviewer-1', kind: 'dag_node', class: 'structured', latest_revision: 4, lineage: { node_id: 'code-reviewer-1', author: 'system' }, revisions: [] },
        { name: 'dag_plan:main', kind: 'dag_plan', class: 'structured', latest_revision: 2, lineage: { node_id: 'orchestrator', author: 'system' }, revisions: [] },
      ],
    })
  }
  return (
    artifactRoute(url, 'code_review:pr:1464', jsonResponse({ data: [{ revision: 2, mime_type: 'application/json', size: 10, kind: 'code_review', class: 'structured', lineage: { node_id: 'code-reviewer-1', author: 'gate' } }] }), textResponse(JSON.stringify(codeReview1464))) ??
    artifactRoute(url, 'finding:89f3e6d1', jsonResponse({ data: [{ revision: 2, mime_type: 'application/json', size: 10, kind: 'finding', class: 'structured', lineage: { node_id: 'code-reviewer-1', author: 'worker' } }] }), textResponse(JSON.stringify(finding89f3e6d1))) ??
    artifactRoute(url, 'finding:e11c2106', jsonResponse({ data: [{ revision: 2, mime_type: 'application/json', size: 10, kind: 'finding', class: 'structured', lineage: { node_id: 'code-reviewer-1', author: 'worker' } }] }), textResponse(JSON.stringify(findingE11c2106))) ??
    artifactRoute(url, 'finding:c1a68ddf', jsonResponse({ data: [{ revision: 2, mime_type: 'application/json', size: 10, kind: 'finding', class: 'structured', lineage: { node_id: 'code-reviewer-1', author: 'worker' } }] }), textResponse(JSON.stringify(findingC1a68ddf))) ??
    artifactRoute(url, 'finding:76f9df59', jsonResponse({ data: [{ revision: 2, mime_type: 'application/json', size: 10, kind: 'finding', class: 'structured', lineage: { node_id: 'code-reviewer-1', author: 'worker' } }] }), textResponse(JSON.stringify(finding76f9df59))) ??
    artifactRoute(url, 'judge_round:e-f957a075-1', jsonResponse({ data: [{ revision: 1, mime_type: 'application/json', size: 10, kind: 'judge_round', class: 'structured', lineage: { node_id: 'code-reviewer-1', author: 'judge' } }] }), textResponse(JSON.stringify(judgeRound1464))) ??
    artifactRoute(url, 'dag_node:code-reviewer-1', jsonResponse({ data: [{ revision: 4, mime_type: 'application/json', size: 10, kind: 'dag_node', class: 'structured', lineage: { node_id: 'code-reviewer-1', author: 'system' } }] }), textResponse(JSON.stringify(dagNode1464))) ??
    artifactRoute(url, 'dag_plan:main', jsonResponse({ data: [{ revision: 2, mime_type: 'application/json', size: 10, kind: 'dag_plan', class: 'structured', lineage: { node_id: 'orchestrator', author: 'system' } }] }), textResponse(JSON.stringify(dagPlan1464))) ??
    null
  )
}

window.fetch = async (input: RequestInfo | URL) => {
  const url = urlOf(input)
  return (
    chatFailedRoute(url) ??
    chatReviewSparseRoute(url) ??
    chatReviewLegacyRoute(url) ??
    chatJudgeOnlyRoute(url) ??
    chatReviewer1466Route(url) ??
    chatMoreRoute(url) ??
    chatOneRoute(url) ??
    jsonResponse({ data: [] })
  )
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

// A code_review always outranks the node's declared kind ('document' here);
// bytes:logo (dispatch bookkeeping) never appears in the secondary list at all.
export const SecondaryHeavyNode: Story = {
  args: {
    chatId: 'chat-more',
    nodeId: 'writer-1',
    nodeAgent: 'Writer',
    nodeTask: 'Write the spec',
    nodeArtifactKind: 'document',
    onClose: () => {},
  },
}

// A minimal review record - verdict only, no takeaway/verified/notes/
// findings - renders via ReviewView with those sections simply absent, never a blank generic tree.
export const SparseCodeReview: Story = {
  args: {
    chatId: 'chat-review-sparse',
    nodeId: 'reviewer-1',
    nodeAgent: 'Code Reviewer',
    nodeTask: 'Review PR #1170',
    nodeArtifactKind: 'code_review',
    onClose: () => {},
  },
}

// A pre-migration review record (`summary`, no `takeaway`/`rendered`) -
// ReviewView falls back to `summary` instead of an empty card.
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

// A code-reviewer node with all five kinds plus the run's own bookkeeping -
// the panel opens on the review, never dag_node/dag_plan.
export const CodeReviewerAllKinds: Story = {
  args: {
    chatId: 'chat-reviewer-1466',
    nodeId: 'code-reviewer-1',
    nodeAgent: 'Code Reviewer',
    nodeTask: 'Review PR #1464 (quack repo, fagerbergj/quack)',
    onClose: () => {},
  },
}

// The orchestrator's own panel (same chat as CodeReviewerAllKinds) - opens on PlanView's assignment list.
export const OrchestratorNode: Story = {
  args: {
    chatId: 'chat-reviewer-1466',
    nodeId: 'orchestrator',
    nodeAgent: 'Orchestrator',
    nodeTask: 'Plan and dispatch the run',
    onClose: () => {},
  },
}

// An ACP implementer that delivers through git and writes no artifact at
// all - the empty state renders the FULL vetted answer as markdown (a one-line caption names the "no artifact" fact, not the content).
export const ImplementerDeliveredNoArtifact: Story = {
  args: {
    chatId: 'chat-failed',
    nodeId: 'implementer-1',
    nodeAgent: 'Code Implementer',
    nodeTask: 'Implement the fix and open a PR',
    nodeAnswer: 'Opened PR #1464 with the sandboxed-git skip and the EnforcesBoundary reuse.\n\n' +
      '- Added a `ResolveSandbox` probe before the git golden fixture, skipping loudly where bwrap is unusable\n' +
      '- Replaced the inline `Sandboxed` predicate with `workspace.EnforcesBoundary`',
    onClose: () => {},
  },
}

// No artifact and no answer either - just two judge rounds. Tapping either
// row opens JudgeRoundView; the chips alone would otherwise be dead ends.
export const NoArtifactWithJudgeRounds: Story = {
  args: {
    chatId: 'chat-judge-only',
    nodeId: 'writer-1',
    nodeAgent: 'Writer',
    nodeTask: 'Write the spec',
    onClose: () => {},
  },
}
