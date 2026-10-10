import type { Meta, StoryObj } from '@storybook/react-vite'
import { within, userEvent } from 'storybook/test'
import { TriggerMessage } from './TriggerEnvelope'

const meta: Meta<typeof TriggerMessage> = {
  title: 'Chat/TriggerEnvelope',
  component: TriggerMessage,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof TriggerMessage>

// A CI-fix trigger: permissions, deliverable and the PR stay visible; everything else is collapsed.
export const CiFix: Story = {
  args: {
    content: `<permissions>push_commits_to_pr, join_pr_conversation</permissions>
<deliverable>commits on this PR's head branch that make the failing checks pass</deliverable>
<pull_request number="97">
  <title>Material 3 theming with dynamic color and dark mode</title>
  <description>Adds dynamic color + dark mode support via Material You.

Closes #65</description>
</pull_request>
<comments new="1" edited="0" deleted="0">[{"id":5181177234,"created_at":"2026-08-04T16:44:10Z","user":{"login":"fagerbergj"},"body":"the BAC status colors fail contrast on the dark surface - fix those"}]</comments>
<changed_files count="16" additions="880" deletions="330">[{"filename":"app/src/main/java/Theme.kt","additions":40,"deletions":5},{"filename":"app/src/main/java/Color.kt","additions":12,"deletions":2}]</changed_files>
<event name="workflow_run.completed">{"action":"completed","workflow_run":{"name":"build","conclusion":"failure","head_sha":"9c8d7e6","head_branch":"quack/issue-65"},"repository":{"full_name":"fagerbergj/NightsOut"}}</event>
<context dir="/workspace/ctx-github-nightsout-97">
  <file name="check-runs.json">GET /repos/fagerbergj/NightsOut/commits/9c8d7e6/check-runs?status=completed</file>
  <file name="annotations-build.json">GET /repos/fagerbergj/NightsOut/check-runs/{id}/annotations</file>
  <file name="files.json">GET /repos/fagerbergj/NightsOut/pulls/97/files</file>
</context>`,
  },
}

// A plan trigger (Step 1): an <issue>, no changed_files (issues don't have them).
export const IssuePlan: Story = {
  args: {
    content: `<permissions>join_issue_conversation</permissions>
<deliverable>an implementation plan, posted to the issue as your answer text</deliverable>
<issue number="65">
  <title>Material 3 theming with dynamic color and dark mode</title>
  <description>The app should adopt Material You dynamic color and support a proper dark theme, following the current Material 3 guidelines.</description>
</issue>
<comments count="2">[{"id":1,"created_at":"2026-08-01T10:00:00Z","user":{"login":"fagerbergj"},"body":"Should this also cover the widget?"},{"id":2,"created_at":"2026-08-01T11:00:00Z","user":{"login":"fagerbergj"},"body":"Never mind, out of scope for now."}]</comments>
<event name="issues.labeled">{"action":"labeled","label":{"name":"quack:plan"},"issue":{"number":65,"state":"open"}}</event>
<context dir="/workspace/ctx-github-nightsout-65"/>`,
  },
}

// Header uses the backend's failing/pending/passing summary; failing checks read in the danger token.
export const ChecksMixedStatuses: Story = {
  args: {
    content: `<permissions>push_commits_to_pr, join_pr_conversation</permissions>
<deliverable>commits on this PR's head branch that make the failing checks pass</deliverable>
<pull_request number="97"><title>Material 3 theming with dynamic color and dark mode</title><description>Implements Material You theming.</description></pull_request>
<checks count="8" summary="2 failing, 1 pending, 5 passing">build (ubuntu-latest): completed failure
build (macos-latest): completed success
lint: completed failure
unit-tests: completed success
integration-tests: completed success
e2e: in_progress
typecheck: completed success
format-check: completed success
</checks>
<event name="check_suite.completed">{"action":"completed","check_suite":{"conclusion":"failure"}}</event>`,
  },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByText('checks: 2 failing, 1 pending, 5 passing'))
  },
}

// A malformed <checks> body (backend contract drift, or truncation) -
// degrades to the raw fallback instead of throwing or dropping the section.
export const MalformedChecks: Story = {
  args: {
    content: `<permissions>join_pr_conversation</permissions>
<deliverable>a review</deliverable>
<checks count="1" summary="1 passing">this is not a valid check line</checks>`,
  },
}

// A block type newer than this UI renders as a labelled raw section, never dropped.
export const UnknownBlock: Story = {
  args: {
    content: `<permissions>join_pr_conversation</permissions>
<deliverable>a review with inline comments and a verdict</deliverable>
<pull_request number="12"><title>Add offline cache</title><description>Caches responses for offline use.</description></pull_request>
<workspace_summary lines="3">3 files touched in internal/cache, no schema changes</workspace_summary>
<event name="pull_request.opened">{"action":"opened","pull_request":{"number":12}}</event>`,
  },
}

// A 40-comment thread: collapsed by default so it can't blow out the message
// before the viewer opens it, height-locked via Expandable once opened.
export const LongCommentThread: Story = {
  args: {
    content: `<permissions>join_pr_conversation</permissions>
<deliverable>a review of what is new since the last one</deliverable>
<comments count="40">${JSON.stringify(
      Array.from({ length: 40 }, (_, i) => ({
        id: i,
        created_at: '2026-08-04T00:00:00Z',
        user: { login: `reviewer${i % 5}` },
        body: `Comment #${i}: this is a note about the change.`,
      })),
    )}</comments>`,
  },
}

// Malformed content (a truncated <comments> body) - degrades to the raw
// fallback instead of throwing or blanking the message.
export const MalformedComments: Story = {
  args: {
    content: `<permissions>join_pr_conversation</permissions>
<deliverable>a review</deliverable>
<comments count="1">{"id":1,"body":"this JSON is truncated and won't par</comments>`,
  },
}

// A plain, non-GitHub chat message: no envelope, renders exactly as it does today.
export const PlainMessage: Story = {
  args: {
    content: 'Can you help me refactor this component to use hooks?',
  },
}

// An A2UI button press persists as a one-line action turn; it renders as a pill.
export const A2uiActionTurn: Story = {
  args: {
    content: '[a2ui_action] {"surface_id":"pr-1085-tutor","name":"submit_quiz","source_component_id":"submit","context":{"answers":{"q1":["b"],"q2":["c"]}}}',
  },
}

// A deleted delta comment must read as retracted, not as a live one.
export const CommentStatuses: Story = {
  args: {
    content: `<permissions>join_pr_conversation</permissions>
<deliverable>a review of what is new since the last one</deliverable>
<comments new="1" edited="1" deleted="1">${JSON.stringify([
      { id: 1, created_at: '2026-08-04T10:00:00Z', user: { login: 'alice' }, body: 'This looks good to me.', quack_status: 'new' },
      { id: 2, created_at: '2026-08-04T10:05:00Z', user: { login: 'bob' }, body: 'Actually, please also update the docs.', quack_status: 'edited' },
      { id: 3, created_at: '2026-08-04T10:10:00Z', user: { login: 'carol' }, body: 'Wait, ignore my earlier comment.', quack_status: 'deleted' },
    ])}</comments>`,
  },
}

// An <event> body that isn't valid JSON degrades to raw text instead of throwing or dropping the block.
export const InvalidEventJson: Story = {
  args: {
    content: `<permissions>join_issue_conversation</permissions>
<deliverable>an answer to their message, posted to the issue as a comment</deliverable>
<event name="issue_comment.created">{not valid json at all</event>`,
  },
}

// Top-level primitive fields render as a grid above the full JSON, which keeps the nested objects.
export const EventFieldsGrid: Story = {
  args: {
    content: `<permissions>join_pr_conversation</permissions>
<deliverable>a review with inline comments and a verdict</deliverable>
<pull_request number="55"><title>Add retry backoff</title><description>Adds exponential backoff to the retry loop.</description></pull_request>
<event name="pull_request.synchronize">{"action":"synchronize","number":55,"before":"a1b2c3d","after":"e4f5a6b","sender_login":"fagerbergj","repository_full_name":"fagerbergj/quack"}</event>`,
  },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByText('pull_request.synchronize'))
  },
}

// A long description collapses behind Show more; a short one (CiFix above) gets no control.
export const LongDescriptionCollapses: Story = {
  args: {
    content: `<permissions>join_issue_conversation</permissions>
<deliverable>an implementation plan, posted to the issue as your answer text</deliverable>
<issue number="88">
  <title>Add offline queueing for outbound webhooks</title>
  <description>${'Outbound webhook deliveries currently fail hard the instant the receiving endpoint is unreachable, dropping the event on the floor with only a log line to show for it. '.repeat(6)}</description>
</issue>
<event name="issues.labeled">{"action":"labeled"}</event>`,
  },
}

// Bodies are seeded verbatim, unescaped, so a literal "<" must survive the hand-rolled tag matcher.
export const LiteralAngleBracketInBody: Story = {
  args: {
    content: `<permissions>join_pr_conversation</permissions>
<deliverable>a review with inline comments and a verdict</deliverable>
<pull_request number="41">
  <title>Add generic Result<T, E> type</title>
  <description>Introduces \`Result<T, E>\` for fallible ops. See \`if x < y { ... }\` in the diff.</description>
</pull_request>
<comments count="1">${JSON.stringify([
      { id: 1, created_at: '2026-08-04T10:00:00Z', user: { login: 'dave' }, body: 'Why not use `Option<T>` here? Also check `a<b && c>d` in the old code.' },
    ])}</comments>`,
  },
}

// An unterminated tag takes the rest of the string as content and stops, never blanking the message.
export const TruncatedTags: Story = {
  args: {
    content: `<permissions>join_pr_conversation</permissions>
<deliverable>a review</deliverable>
<pull_request number="9"><title>Unterminated title and no closing tags at all`,
  },
}

// A resumed trigger: the body shows the issue's running history across three triggers, while the header
// still reports this turn's own delta ("1 new, 0 edited, 0 deleted").
export const AccumulatedCommentHistory: Story = {
  args: {
    content: `<permissions>join_issue_conversation</permissions>
<deliverable>an answer to their message, posted to the issue as a comment</deliverable>
<issue number="88">
  <title>Dark mode toggle flickers on first load</title>
  <description>The toggle briefly shows the wrong state before settling on the persisted theme.</description>
</issue>
<comments new="1" edited="0" deleted="0">${JSON.stringify([
      { id: 4, created_at: '2026-08-05T09:40:00Z', user: { login: 'dave' }, body: "One more thing - it's worse on Safari.", quack_status: 'new' },
    ])}</comments>
<event name="issue_comment.created">{"action":"created","comment":{"id":4}}</event>`,
    priorContents: [
      `<permissions>join_issue_conversation</permissions>
<deliverable>an answer to their message</deliverable>
<issue number="88"><title>Dark mode toggle flickers on first load</title><description>The toggle briefly shows the wrong state before settling on the persisted theme.</description></issue>
<comments count="2">${JSON.stringify([
        { id: 1, created_at: '2026-08-05T09:00:00Z', user: { login: 'alice' }, body: 'Repro: reload with system theme set to dark, watch the toggle flash light first.' },
        { id: 2, created_at: '2026-08-05T09:10:00Z', user: { login: 'bob' }, body: 'Confirmed on Chrome and Firefox.' },
      ])}</comments>`,
      `<permissions>join_issue_conversation</permissions>
<deliverable>an answer to their message</deliverable>
<issue number="88"><title>Dark mode toggle flickers on first load</title><description>The toggle briefly shows the wrong state before settling on the persisted theme.</description></issue>
<comments new="1" edited="0" deleted="0">${JSON.stringify([
        { id: 3, created_at: '2026-08-05T09:25:00Z', user: { login: 'carol' }, body: 'Likely a hydration mismatch - the SSR shell renders before localStorage is read.', quack_status: 'new' },
      ])}</comments>`,
    ],
  },
}

// No seed turn is visible (reaped context or rehydrated store), so the UI must flag incomplete history.
export const IncompleteCommentHistory: Story = {
  args: {
    content: `<permissions>join_pr_conversation</permissions>
<deliverable>a review of what is new since the last one</deliverable>
<pull_request number="112"><title>Add retry backoff to the sync client</title><description>Adds capped exponential backoff around the sync client's retry loop.</description></pull_request>
<comments new="1" edited="0" deleted="0">${JSON.stringify([
      { id: 9, created_at: '2026-08-05T14:00:00Z', user: { login: 'erin' }, body: 'Can you also cap the jitter window? It can currently exceed the base delay.', quack_status: 'new' },
    ])}</comments>
<changed_files count="1" additions="12" deletions="3">${JSON.stringify([{ filename: 'internal/sync/retry.go', additions: 12, deletions: 3 }])}</changed_files>
<event name="issue_comment.created">{"action":"created","comment":{"id":9}}</event>`,
  },
}

// A seed turn with no comments opens to "no comments", not an incomplete-history notice.
export const EmptyCommentHistory: Story = {
  args: {
    content: `<permissions>join_issue_conversation</permissions>
<deliverable>an implementation plan, posted to the issue as your answer text</deliverable>
<issue number="120">
  <title>Add CSV export to the reports page</title>
  <description>Users want to download the current report view as a CSV file.</description>
</issue>
<comments count="0">[]</comments>
<event name="issues.labeled">{"action":"labeled","label":{"name":"quack:plan"}}</event>`,
  },
}

// The <artifacts> block renders as a compact row list, not a raw XML code block.
export const Artifacts: Story = {
  args: {
    content: `<permissions>push_commits_to_pr, join_pr_conversation</permissions>
<deliverable>commits on this PR's head branch that make the failing checks pass</deliverable>
<pull_request number="97"><title>Material 3 theming with dynamic color and dark mode</title><description>Adds dynamic color + dark mode support via Material You.</description></pull_request>
<artifacts>
  <artifact id="bytes:comments" revision="1" status="new">1 items</artifact>
  <artifact id="bytes:commits" revision="1" status="new">1 items</artifact>
  <artifact id="bytes:event" revision="1" status="new">pull_request.labeled</artifact>
  <artifact id="bytes:files" revision="1" status="new">5 items</artifact>
  <artifact id="bytes:issue" revision="1" status="new">1 object</artifact>
  <artifact id="bytes:linked-issue-1248" revision="1" status="new">1 object</artifact>
</artifacts>`,
  },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByText('6 artifacts'))
  },
}

// Wide unbroken content must scroll inside its own container, never the page, even at ~380px.
export const WideContent: Story = {
  args: {
    content: `<permissions>push_commits_to_pr</permissions>
<deliverable>commits on this PR's head branch that make the failing checks pass</deliverable>
<pull_request number="200"><title>Refactor</title><description>See app/src/main/java/com/example/nightsout/features/theming/dynamic/color/extraction/palette/generator/DynamicPaletteGenerator.kt</description></pull_request>
<changed_files count="1" additions="1" deletions="1">[{"filename":"app/src/main/java/com/example/nightsout/features/theming/dynamic/color/extraction/palette/generator/DynamicPaletteGenerator.kt","additions":1,"deletions":1}]</changed_files>
<comments count="1">${JSON.stringify([
      { id: 1, created_at: '2026-08-04T10:00:00Z', user: { login: 'ci-bot' }, body: 'Failing at app/src/main/java/com/example/nightsout/features/theming/dynamic/color/extraction/palette/generator/DynamicPaletteGeneratorVeryLongTestClassNameThatWontBreak.kt:142' },
    ])}</comments>
<event name="workflow_run.completed">{"action":"completed","note":"aVeryLongUnbrokenSingleTokenValueThatCouldForceThePageToScrollSidewaysIfNotContainedProperlyWithinItsOwnScrollableCodeBlockContainer"}</event>
<context dir="/workspace/ctx-github-nightsout-200">
  <file name="check-runs.json">GET /repos/fagerbergj/NightsOut/commits/9c8d7e6f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d/check-runs?status=completed&per_page=100&some_extra_query_param=verylongvalue</file>
</context>`,
  },
}
