import type { Meta, StoryObj } from '@storybook/react-vite'
import { ActivityList, AssistantText } from './AgentParts'
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
