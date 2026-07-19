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

// In-flight tool call (no result yet) renders a "running…" indicator.
export const ToolRunning: Story = {
  args: {
    activity: [
      { kind: 'thinking', text: 'Searching for Dublin climate data…' },
      { kind: 'tool', tool: { callId: 'c1', name: 'web_search', args: { query: 'Dublin weather' }, done: false } },
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
