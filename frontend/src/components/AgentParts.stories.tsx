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

// A dispatch tree: the orchestrator reasons, then dispatches web-researcher, whose
// activity nests inside it; the answer renders at the top level.
const dispatchTree: MessagePart[] = [
  {
    kind: 'agent',
    agent: 'orchestrator',
    done: true,
    items: [
      { kind: 'thinking', text: 'This needs live web research — dispatch the web-researcher.' },
      {
        kind: 'agent',
        agent: 'web-researcher',
        done: true,
        items: [
          { kind: 'thinking', text: 'Search for current events in Dublin, then read the top sources.' },
          { kind: 'tool_call', name: 'web_search', args: { query: 'things to do in Dublin summer' }, result: { results: [{ title: 'Dublin events', url: 'https://example.com' }] } },
          { kind: 'tool_call', name: 'web_fetch', args: { url: 'https://example.com' }, result: 'Dublin in summer offers…' },
        ],
      },
    ],
  },
  { kind: 'text', text: 'Visit the [Guinness Storehouse](https://example.com) and stroll [Phoenix Park](https://example.com).' },
]

export const DispatchTree: Story = {
  args: { parts: dispatchTree },
}

// The trust gate: the researcher works, self-refines, the judge fails round 1
// then passes round 2, and the vetted answer renders at the top level.
const vettedTree: MessagePart[] = [
  {
    kind: 'agent',
    agent: 'web-researcher',
    done: true,
    items: [
      { kind: 'tool_call', name: 'web_search', args: { query: 'best time to visit Dublin' }, result: { results: [] } },
      {
        kind: 'self_refine',
        changed: true,
        done: true,
        items: [{ kind: 'thinking', text: 'I should add a citation for the weather claim.' }],
      },
      {
        kind: 'judge_verdict',
        round: 1,
        score: 0.45,
        passed: false,
        feedback: 'Claims about weather lack a cited source; add one.',
        done: true,
        items: [{ kind: 'thinking', text: 'The answer is mostly well-structured but the weather claim on line 2 lacks a source URL.' }],
      },
      { kind: 'revise', round: 1 },
      {
        kind: 'judge_verdict',
        round: 2,
        score: 0.86,
        passed: true,
        feedback: '',
        done: true,
        items: [{ kind: 'thinking', text: 'All claims now have sources. Score: 0.86.' }],
      },
    ],
  },
  { kind: 'text', text: 'The best time to visit Dublin is **May–September**, per [Failte Ireland](https://example.com).' },
]

export const VettedAnswer: Story = {
  args: { parts: vettedTree },
}

// A long-running agent: with more than 3 activity items, older ones fold behind a
// "⋯ N earlier" toggle.
export const WindowedActivity: Story = {
  args: {
    parts: [
      {
        kind: 'agent',
        agent: 'web-researcher',
        done: false,
        items: [
          { kind: 'thinking', text: 'first plan' },
          { kind: 'tool_call', name: 'web_search', args: { query: 'a' }, result: { results: [] } },
          { kind: 'tool_call', name: 'web_fetch', args: { url: 'https://1' }, result: '…' },
          { kind: 'thinking', text: 'refining' },
          { kind: 'tool_call', name: 'web_search', args: { query: 'b' }, result: { results: [] } },
          { kind: 'tool_call', name: 'web_fetch', args: { url: 'https://2' } },
        ],
      },
    ],
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
