import type { Meta, StoryObj } from '@storybook/react-vite'
import { within, userEvent, expect, waitFor } from 'storybook/test'
import { ActivityList, AssistantText, BubbleHeader, LiveStatusLine } from './AgentParts'
import { appendRunThinking, appendRunToolCall, fillRunToolResult, startRun } from './messageParts'
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

// A failed tool call shows a cross, not a check: icon plus colour, never colour alone.
export const ToolFailed: Story = {
  args: {
    activity: [
      { kind: 'tool', tool: { callId: 'c1', name: 'run_command', args: { command: 'go test ./...' }, result: { error: 'exit status 1' }, done: true } },
    ],
  },
}

// An ACP file edit maps to edit_file and gets the same before/after diff as a native edit.
// A new file (no prior content) shows every line as added.
export const AcpEditFileDiff: Story = {
  args: {
    activity: [
      { kind: 'thinking', text: 'The bug is in the debounce timer - it never clears on unmount.' },
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

// The native edit_file call must look identical to the ACP one above.
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

// Toggle Storybook's dark-mode control to check the Thought SVG renders cleanly in both themes.
export const ThinkBlockIcon: Story = {
  args: {
    activity: [
      { kind: 'thinking', text: 'Checking both themes: this icon is an inline SVG using currentColor, so it should look crisp and correctly muted whether the page is light or dark.' },
    ],
  },
}

// Tool rows carry no copy button; the row still expands to ToolCallView's full detail.
export const ToolCallNoCopyButton: Story = {
  args: {
    activity: [
      { kind: 'tool', tool: { callId: 'c1', name: 'run_command', done: true, args: { command: 'go test ./...' }, result: { exit_code: 0, output: 'ok' } } },
    ],
  },
  play: async ({ canvasElement }) => {
    expect(canvasElement.querySelector('button[aria-label^="Copy"]')).toBeNull()
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

// Many tool-call events: ActivityList windows to the most recent RECENT items so this stays cheap.
const manyActivity: Activity[] = Array.from({ length: 60 }, (_, i) => ({
  kind: 'tool' as const,
  tool: { callId: `c${i}`, name: 'web_search', args: { query: `dublin weather query ${i}` }, result: { results: [] }, done: true },
}))

export const ManyToolCalls: Story = {
  args: { activity: manyActivity },
}

// Interleaved thinking/tool-call events fold into one Thought block.
function buildInterleavedFixture(): Activity[] {
  let runs = startRun([], { runId: 'r1', agent: 'code-reviewer', stage: 'worker' })
  const fragments = [
    'Let me look at the diff first.', 'OK so this touches the auth middleware.',
    'Checking for missing nil checks.', 'This looks fine.', 'Now the tests.',
    'Coverage seems thin here.', 'Let me check the error path too.',
    'Good, that is handled.', 'One more file to check.', 'This is the last one.',
  ]
  for (let i = 0; i < fragments.length; i++) {
    runs = appendRunThinking(runs, 'r1', fragments[i])
    const callId = `c${i}`
    // i===3/7 are bridged MCP calls; the relay resolves their real names, so the fixture never builds "other".
    const isBridged = i === 3 || i === 7
    const name = isBridged ? (i === 3 ? 'stage_review' : 'load_skill') : 'read_file'
    runs = appendRunToolCall(runs, 'r1', callId, name, isBridged ? {} : { path: `src/file${i}.go` })
    runs = fillRunToolResult(runs, 'r1', callId, name, { ok: true })
  }
  return runs[0].activity
}

export const InterleavedThinkingAndOtherTools: Story = {
  args: { activity: buildInterleavedFixture() },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    // The folded Thought block is the run's oldest item, so it's behind the
    // windowing toggle - expand it before asserting.
    await userEvent.click(canvas.getByText(/earlier/))
    // All ten thought fragments folded into one Thought block, not ten.
    expect(canvas.getAllByText('Thought')).toHaveLength(1)
    // Every tool row shows its real identity - never the bare "other".
    expect(canvas.queryByText('other')).toBeNull()
    expect(canvas.getByText('stage_review')).toBeInTheDocument()
    expect(canvas.getByText('load_skill')).toBeInTheDocument()
  },
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

// A single-backtick span renders as inline code, distinct from the fenced block that follows.
const INLINE_CODE_ANSWER = `Set \`QUACK_LOG_LEVEL\` to \`debug\` in the environment, then restart with \`make docker-up\`. The default is \`info\`.

\`\`\`bash
QUACK_LOG_LEVEL=debug make docker-up
\`\`\``

export const WithInlineCode: Story = {
  render: () => <AssistantText text={INLINE_CODE_ANSWER} />,
}

// A bare punctuation backtick earlier in the paragraph would defeat CommonMark's backtick pairing
// without backticks.ts's fix.
export const InlineCodeAfterStrayBacktick: Story = {
  render: () => <AssistantText text={"Don't use a bare ` unless needed. Instead set `QUACK_LOG_LEVEL` to `debug`."} />,
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    const code = canvas.getByText('QUACK_LOG_LEVEL')
    expect(code.tagName).toBe('CODE')
    expect(canvas.getByText('debug').tagName).toBe('CODE')
  },
}

// A closed mermaid fence renders as a diagram; mermaid lazy-loads, so it appears a beat after the bubble.
const MERMAID_VALID = `Here's the request flow:

\`\`\`mermaid
flowchart TD
  A[Client] --> B[Router]
  B --> C[Orchestrator]
  C --> D[DAG node]
\`\`\`

Each node runs through the trust gate before its output propagates.`

export const MermaidValid: Story = {
  render: () => <AssistantText text={MERMAID_VALID} />,
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await waitFor(() => expect(canvas.getByTestId('mermaid-diagram').querySelector('svg')).not.toBeNull(), { timeout: 5000 })
  },
}

// Invalid mermaid falls back to a plain code block plus an inline notice, never a throw or blank bubble.
const MERMAID_INVALID = `\`\`\`mermaid
this is not a valid diagram @@@ %%%
\`\`\``

export const MermaidInvalid: Story = {
  render: () => <AssistantText text={MERMAID_INVALID} />,
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await waitFor(() => expect(canvas.getByText(/Diagram failed to render/)).toBeInTheDocument(), { timeout: 5000 })
    expect(canvas.queryByTestId('mermaid-diagram')).toBeNull()
  },
}

// An unterminated mermaid fence, as while streaming: a plain code block, no error flash, until it closes.
const MERMAID_STREAMING = `Here's the request flow:

\`\`\`mermaid
flowchart TD
  A[Client] --> B[Router`

export const MermaidStreaming: Story = {
  render: () => <AssistantText text={MERMAID_STREAMING} />,
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    expect(canvas.queryByTestId('mermaid-diagram')).toBeNull()
    expect(canvas.queryByText(/Diagram failed to render/)).toBeNull()
  },
}

// The store resets the answer accumulator on each tool call, so only text after the last call renders.
const preambleActivity: Activity[] = [
  { kind: 'thinking', text: 'The user wants the timeout value - I should read the config rather than guess.' },
  { kind: 'tool', tool: { callId: 'c1', name: 'read_file', args: { path: 'config.yaml' }, result: { content: 'timeout: 30s' }, done: true } },
]

// Only a live orchestrator card shows a StatusDot before the name; a completed turn passes no `status`.
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
        answer - narration before the tool call above never lands here
      </div>
      <AssistantText text="The configured timeout is 30 seconds." />
    </div>
  ),
}

// What a running run shows instead of ActivityList: both lines, each alone, and nothing.
export const LiveStatusThinkingAndTool: Story = {
  render: () => (
    <div className="max-w-lg rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 p-3">
      <LiveStatusLine
        activity={[
          { kind: 'tool', tool: { callId: 'c1', name: 'edit_file', args: { path: 'app/src/main/java/com/wit/nightsout/settings/SettingsScreen.kt' }, done: true } },
          { kind: 'thinking', text: 'The nested verticalScroll is what throws - drop the inner one.' },
        ]}
      />
    </div>
  ),
}

// Tail is a tool call, so no thinking line - the common mid-run shape.
export const LiveStatusToolOnly: Story = {
  render: () => (
    <div className="max-w-lg rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 p-3">
      <LiveStatusLine
        activity={[{ kind: 'tool', tool: { callId: 'c1', name: 'run_command', args: { command: './gradlew connectedAndroidTest' }, done: false } }]}
      />
    </div>
  ),
}

// Reasoning before any tool has been called.
export const LiveStatusThinkingOnly: Story = {
  render: () => (
    <div className="max-w-lg rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 p-3">
      <LiveStatusLine activity={[{ kind: 'thinking', text: 'Reading the failing CI log first.' }]} />
    </div>
  ),
}

// An unmapped tool name degrades to the raw name rather than inventing a verb.
export const LiveStatusUnmappedTool: Story = {
  render: () => (
    <div className="max-w-lg rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 p-3">
      <LiveStatusLine
        activity={[{ kind: 'tool', tool: { callId: 'c1', name: 'load_skill', args: { name: 'ponytail' }, done: true } }]}
      />
    </div>
  ),
}

// Assistant prose fills its column; a wide table or a long code line still
// scrolls inside its own box rather than widening the bubble.
export const ProseWideContent: StoryObj = {
  parameters: { renderCheck: { viewports: ['mobile', 'desktop'] } },
  render: () => (
    <AssistantText text={[
      'Here is the comparison you asked for, with one long paragraph first so the measure is visible: Dublin is mild year-round, May to September is warmest at fifteen to eighteen degrees, and the rain is spread evenly enough that no month is reliably dry, which is why most guides recommend late spring for the best trade-off between daylight, temperature and crowds.',
      '',
      '| Month | Avg high | Avg low | Rain days | Daylight | Crowds | Hotel price index | Notes |',
      '| --- | --- | --- | --- | --- | --- | --- | --- |',
      '| May | 15 °C | 7 °C | 11 | 16h | moderate | 105 | best trade-off between weather, daylight and price |',
      '| August | 19 °C | 12 °C | 12 | 15h | high | 130 | peak season, book early |',
      '',
      '```sh',
      'curl -s "https://example.com/api/v1/climate?city=dublin&months=may,june,july,august,september&fields=high,low,rain_days,daylight" | jq .',
      '```',
    ].join('\n')} />
  ),
}
