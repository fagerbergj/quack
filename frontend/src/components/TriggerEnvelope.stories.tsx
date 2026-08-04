import type { Meta, StoryObj } from '@storybook/react-vite'
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
