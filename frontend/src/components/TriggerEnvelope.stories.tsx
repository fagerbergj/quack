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

// A CI-fix trigger (design: .quack/trigger-prompts-v2.md, Step 6): permissions
// and deliverable always visible, PR title/description expanded, comments/
// changed-files/event/context collapsed behind their summary line.
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

// A block type quack starts emitting after this UI shipped: rendered as a
// labelled collapsed section with its raw content, never dropped (#667).
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

// Delta comments carrying quack_status (internal/github/envelope.go): a
// deleted comment must read as retracted, not as a live one (#667's
// quack_status field exists specifically so a miscount can't hide this).
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

// <event> whose body isn't valid JSON - degrades to the raw text instead of
// throwing or dropping the block (#667).
export const InvalidEventJson: Story = {
  args: {
    content: `<permissions>join_issue_conversation</permissions>
<deliverable>an answer to their message, posted to the issue as a comment</deliverable>
<event name="issue_comment.created">{not valid json at all</event>`,
  },
}

// #746 item 9: the event section's TOP-LEVEL primitive fields render as a
// responsive grid above the full JSON, using the wide pane's width instead of
// one "key: value" per line - the full payload (with its nested objects) is
// still there below, just no longer the only view.
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

// #746 item 8: a long issue/PR description collapses to its first lines with
// a Show more control (a short one, see CiFix above, gets no control at all).
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

// A comment body and PR description containing a literal "<" (the envelope
// seeds GitHub bodies verbatim, unescaped - envelope.ts's own rationale for
// hand-rolled tag matching over a real XML/HTML parser).
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

// Truncated/unterminated tags at multiple levels - takes the rest of the
// string as that block's content and stops, rather than throwing or blanking
// the whole message (envelope.ts's parseTopLevel).
export const TruncatedTags: Story = {
  args: {
    content: `<permissions>join_pr_conversation</permissions>
<deliverable>a review</deliverable>
<pull_request number="9"><title>Unterminated title and no closing tags at all`,
  },
}

// #730: a resumed trigger whose <comments> section shows the ISSUE'S RUNNING
// HISTORY (this turn's delta folded onto every earlier turn it was seeded
// with), not just what this one trigger's envelope carried. The collapsed
// header still reports this turn's own delta ("1 new, 0 edited, 0 deleted")
// - open the section to see all four comments across three triggers.
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

// #730: a chat opened after the run's context was reaped (or a rehydrated
// store) - the earliest turn this client can see is ITSELF a delta, so there
// is no seed to accumulate onto. The UI must say so rather than presenting
// this one comment as if it were the issue's whole thread.
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

// #730 empty state: a fresh issue's seed turn with no comments yet. The
// section opens to a plain "no comments" line, not an empty list or a
// misleading incomplete-history notice.
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

// Wide, unbroken content (a long nested file path, a JSON payload with a long
// single-token value) must scroll inside its own container - never make the
// page itself scroll sideways. Verified at a narrow (~380px) viewport too.
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
