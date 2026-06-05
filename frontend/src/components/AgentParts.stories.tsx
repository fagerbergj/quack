import type { Meta, StoryObj } from '@storybook/react-vite'
import { AssistantParts, type MessagePart } from './AgentParts'

const meta: Meta<typeof AssistantParts> = {
  title: 'Chat/AssistantParts',
  component: AssistantParts,
}
export default meta

type Story = StoryObj<typeof AssistantParts>

// A full assistant turn: reasoning, a completed tool call, then the answer —
// the M0 labeled-event vocabulary as the chat renders it.
const fullTurn: MessagePart[] = [
  {
    kind: 'thinking',
    text: 'The user wants the current time and asked me to use a tool.\nI have `current_time`; I will call it, then format the result.',
  },
  {
    kind: 'tool_call',
    name: 'current_time',
    args: {},
    result: { result: '2026-06-05T01:23:23Z' },
  },
  { kind: 'text', text: 'The current time is **June 5, 2026, at 01:23:23 UTC**.' },
]

export const FullTurn: Story = {
  args: { parts: fullTurn },
}

export const ThinkingOnly: Story = {
  args: {
    parts: [{ kind: 'thinking', text: 'Let me reason about this step by step…' }],
  },
}

export const ToolRunning: Story = {
  args: {
    parts: [{ kind: 'tool_call', name: 'web_search', args: { query: 'best time to visit Dublin' } }],
  },
}

export const Streaming: Story = {
  args: {
    parts: [{ kind: 'text', text: 'Streaming a reply' }],
    showCursor: true,
  },
}

export const Markdown: Story = {
  args: {
    parts: [
      {
        kind: 'text',
        text: '## Plan\n\n1. **Research** the destination\n2. Compare options\n\n```ts\nconst quack = "🦆"\n```\n\n> Quote block, and `inline code`.',
      },
    ],
  },
}
