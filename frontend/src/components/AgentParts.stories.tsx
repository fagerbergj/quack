import type { Meta, StoryObj } from '@storybook/react-vite'
import { within, userEvent, expect } from 'storybook/test'
import { ActivityList, AssistantText, BubbleHeader } from './AgentParts'
import type { Activity } from './messageParts'

const meta: Meta<typeof ActivityList> = {
  title: 'Chat/ActivityList',
  component: ActivityList,
}
export default meta

type Story = StoryObj<typeof ActivityList>

// One run's activity: reasoning interleaved with completed and in-flight tools.
const activity: Activity[] = [
  { kind: 'thinking', text: 'I need the best months to visit Dublin based on weather data.' },
  { kind: 'tool', tool: { callId: 'c1', name: 'web_search', args: { query: 'best time to visit Dublin weather' }, result: { results: [{ title: 'Dublin Climate Guide', url: 'https://example.com/climate' }] }, done: true } },
  { kind: 'tool', tool: { callId: 'c2', name: 'web_fetch', args: { url: 'https://example.com/climate' }, result: 'Dublin is mild year-round; May–September is warmest (15–18 °C).', done: true } },
]

export const Basic: Story = {
  args: { activity },
}

// In-flight tool call (no result yet) renders a "working" dots indicator.
export const ToolRunning: Story = {
  args: {
    activity: [
      { kind: 'thinking', text: 'Searching for Dublin climate data…' },
      { kind: 'tool', tool: { callId: 'c1', name: 'web_search', args: { query: 'Dublin weather' }, done: false } },
    ],
  },
}

// A failed tool call's collapsed summary shows a cross, not a check (#385) —
// status conveyed by icon+colour together, never colour alone.
export const ToolFailed: Story = {
  args: {
    activity: [
      { kind: 'tool', tool: { callId: 'c1', name: 'run_command', args: { command: 'go test ./...' }, result: { error: 'exit status 1' }, done: true } },
    ],
  },
}

// #388 — an ACP code-implementer's file edit now maps to edit_file (not
// write_file) and renders the SAME before→after diff view a native edit gets.
// A new file (no prior content) shows every line as added.
export const AcpEditFileDiff: Story = {
  args: {
    activity: [
      { kind: 'thinking', text: 'The bug is in the debounce timer — it never clears on unmount.' },
      {
        kind: 'tool',
        tool: {
          callId: 'c1', name: 'edit_file', done: true,
          args: { path: 'src/hooks/useDebounce.ts', old: 'useEffect(() => {\n  return () => {}\n}, [])', new: 'useEffect(() => {\n  return () => clearTimeout(t)\n}, [])' },
          result: { replacements: 1 },
        },
      },
    ],
  },
}

// The native code-implementer's own edit_file call — same tool name, same
// diff view, rendered identically to the ACP one above (#388's acceptance:
// ACP and native edits are visually indistinguishable).
export const NativeEditFileDiff: Story = {
  args: {
    activity: [
      {
        kind: 'tool',
        tool: {
          callId: 'c1', name: 'edit_file', done: true,
          args: { path: 'src/hooks/useDebounce.ts', old: 'useEffect(() => {\n  return () => {}\n}, [])', new: 'useEffect(() => {\n  return () => clearTimeout(t)\n}, [])' },
          result: { replacements: 1 },
        },
      },
    ],
  },
}

// The "Thought" icon is a crisp currentColor SVG (not an emoji, which used to
// render pixelated/off-colour on a dark background) — toggle the Storybook
// toolbar's dark-mode control to check it renders cleanly in both themes.
export const ThinkBlockIcon: Story = {
  args: {
    activity: [
      { kind: 'thinking', text: 'Checking both themes: this icon is an inline SVG using currentColor, so it should look crisp and correctly muted whether the page is light or dark.' },
    ],
  },
}

// An ACP code-implementer's tool call carries the "ACP" badge (#404) — best
// effort, external-agent origin, distinct from a native tool call.
export const AcpBadgedCall: Story = {
  args: {
    activity: [
      { kind: 'tool', tool: { callId: 'c1', name: 'edit_file', done: true, args: { path: 'src/hooks/useDebounce.ts', old: 'let t', new: 'let timer' }, result: { replacements: 1 } } },
    ],
    agent: 'code-implementer',
  },
}

// The same call from a NATIVE agent carries no badge — the badge is strictly
// an ACP-origin signal, not a generic "external tool" marker.
export const NativeCallNoBadge: Story = {
  args: {
    activity: [
      { kind: 'tool', tool: { callId: 'c1', name: 'edit_file', done: true, args: { path: 'src/hooks/useDebounce.ts', old: 'let t', new: 'let timer' }, result: { replacements: 1 } } },
    ],
    agent: 'web-researcher',
  },
}

// Every tool call carries a small copy-JSON button (✎ → ✓ on click) in its
// collapsed summary line — the escape hatch for whatever a rendered view
// doesn't show nicely. Click it here (clipboard writes are inert in a
// sandboxed Storybook iframe, but the ✓ flash still confirms the click fired).
export const CopyButtonOnToolCall: Story = {
  args: {
    activity: [
      { kind: 'tool', tool: { callId: 'c1', name: 'run_command', done: true, args: { command: 'go test ./...' }, result: { exit_code: 0, output: 'ok' } } },
    ],
  },
}

// #435 — the copy button sits in a sibling row over the summary's corner,
// not nested inside the `<summary>` (invalid HTML, breaks keyboard use):
// clicking it copies without toggling the collapsed tool block open.
export const CopyButtonDoesNotToggleSummary: Story = {
  args: {
    activity: [
      { kind: 'tool', tool: { callId: 'c1', name: 'run_command', done: true, args: { command: 'go test ./...' }, result: { exit_code: 0, output: 'ok' } } },
    ],
  },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    const summary = canvasElement.querySelector('summary')!
    expect(summary.querySelector('button')).toBeNull()
    const details = canvasElement.querySelector('details')!
    await userEvent.click(canvas.getByLabelText('Copy tool call JSON'))
    expect(details.open).toBe(false)
  },
}

// With more than 3 items, older ones fold behind a "⋯ N earlier" toggle.
export const Windowed: Story = {
  args: {
    activity: [
      { kind: 'thinking', text: 'step 1' },
      { kind: 'tool', tool: { callId: 'a', name: 'web_search', args: { query: 'a' }, result: {}, done: true } },
      { kind: 'tool', tool: { callId: 'b', name: 'web_search', args: { query: 'b' }, result: {}, done: true } },
      { kind: 'tool', tool: { callId: 'c', name: 'web_fetch', args: { url: 'https://example.com' }, result: 'page', done: true } },
      { kind: 'thinking', text: 'now compiling the answer' },
    ],
  },
}

// #379: a run with many tool-call events — the case the streaming-update
// perf fix targets. ActivityList itself already windows to the most recent
// RECENT items, so this stays cheap to render regardless of count; it's here
// to make that windowing (and the underlying store fix) visible/verifiable.
const manyActivity: Activity[] = Array.from({ length: 60 }, (_, i) => ({
  kind: 'tool' as const,
  tool: { callId: `c${i}`, name: 'web_search', args: { query: `dublin weather query ${i}` }, result: { results: [] }, done: true },
}))

export const ManyToolCalls: Story = {
  args: { activity: manyActivity },
}

const CODE_ANSWER = `Here's a debounce helper:

\`\`\`ts
function debounce<T extends (...a: never[]) => void>(fn: T, ms: number) {
  let t: ReturnType<typeof setTimeout>
  return (...args: Parameters<T>) => {
    clearTimeout(t)
    t = setTimeout(() => fn(...args), ms)
  }
}
\`\`\`

It coalesces rapid calls into the trailing one.`

// AssistantText with a fenced code block: syntax-highlighted (rehype-highlight)
// with a hover Copy button (CopyablePre). Hover the block to reveal Copy.
export const WithCodeBlock: Story = {
  render: () => <AssistantText text={CODE_ANSWER} />,
}

// #387 — preamble/reasoning tokens are never the answer. The activity list
// (collapsed "Thought" + tool calls) is visually distinct FROM and sits ABOVE
// the answer bubble; narration a worker emitted before its tool call ("Let me
// check the config first…") never reaches the answer at all — the store
// resets that accumulator on each tool call (chatStore.ts), so only the text
// after the last tool call ever renders as "the answer" below.
const preambleActivity: Activity[] = [
  { kind: 'thinking', text: 'The user wants the timeout value — I should read the config rather than guess.' },
  { kind: 'tool', tool: { callId: 'c1', name: 'read_file', args: { path: 'config.yaml' }, result: { content: 'timeout: 30s' }, done: true } },
]

// #416 — the top-level orchestrator card's BubbleHeader carries a StatusDot to
// the left of the name while the turn is live, matching DagNode's header
// (dot-then-name); a completed turn passes no `status` and shows no dot.
export const OrchestratorCardRunning: Story = {
  render: () => (
    <div className="max-w-lg rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 p-3">
      <BubbleHeader agent="orchestrator" status="running" />
      <ActivityList activity={preambleActivity} />
    </div>
  ),
}

export const OrchestratorCardDone: Story = {
  render: () => (
    <div className="max-w-lg rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 p-3">
      <BubbleHeader agent="orchestrator" status="done" model="gpt-oss-120b" tokens={412} />
      <AssistantText text="Dublin's warmest months are May through September." />
    </div>
  ),
}

export const PreambleVsAnswer: Story = {
  render: () => (
    <div className="max-w-lg rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 p-3 space-y-2">
      <div className="text-[10px] uppercase tracking-wide text-gray-400 dark:text-gray-500">activity (reasoning + tool calls)</div>
      <ActivityList activity={preambleActivity} />
      <div className="text-[10px] uppercase tracking-wide text-gray-400 dark:text-gray-500 pt-2 border-t border-gray-100 dark:border-gray-700">
        answer — narration before the tool call above never lands here
      </div>
      <AssistantText text="The configured timeout is 30 seconds." />
    </div>
  ),
}
