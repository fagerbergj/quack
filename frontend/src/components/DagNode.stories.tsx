import type { Meta, StoryObj } from '@storybook/react-vite'
import { DagNode } from './DagNode'
import type { DagNodeDef } from '../state/agentStream'
import type { MessagePart } from './messageParts'

const meta: Meta<typeof DagNode> = {
  title: 'Chat/DagNode',
  component: DagNode,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof DagNode>

// ---- node definitions -------------------------------------------------------

const wrNode: DagNodeDef = {
  id: 'r1',
  agent: 'web-researcher',
  task: 'Find the best time to visit Dublin: climate, peak/off-peak seasons, and rainfall data.',
  depends_on: [],
}

const synthNode: DagNodeDef = {
  id: 'synth',
  agent: 'synthesizer',
  task: 'Combine the research into a concise Dublin travel guide.',
  depends_on: ['r1'],
}

// ---- part fixtures ----------------------------------------------------------

const researchParts: MessagePart[] = [
  { kind: 'thinking', text: 'I need to find the best months to visit Dublin based on weather data.' },
  { kind: 'tool_call', name: 'web_search', args: { query: 'best time to visit Dublin weather' }, result: { results: [{ title: 'Dublin Climate Guide', url: 'https://example.com/climate' }] } },
  { kind: 'tool_call', name: 'web_fetch', args: { url: 'https://example.com/climate' }, result: 'Dublin has mild temperatures year-round. May–September is warmest (15–18 °C).' },
  { kind: 'text', text: 'The best months to visit Dublin are **May through September**, particularly June–August for warmest weather and longest daylight hours.' },
]

const withSelfCritParts: MessagePart[] = [
  { kind: 'tool_call', name: 'web_search', args: { query: 'Dublin weather best months' }, result: { results: [] } },
  {
    kind: 'self_refine',
    changed: true,
    done: true,
    startedAt: 0,
    durationMs: 4_200,
    items: [{ kind: 'thinking', text: 'I should add a source URL for the weather claim.' }],
  },
  { kind: 'text', text: 'Best time to visit Dublin is **May–September**, per [Met Éireann](https://example.com/met).' },
]

const withJudgeRoundsParts: MessagePart[] = [
  { kind: 'tool_call', name: 'web_search', args: { query: 'Dublin weather' }, result: { results: [] } },
  {
    kind: 'self_refine',
    changed: false,
    done: true,
    startedAt: 0,
    durationMs: 3_100,
    items: [],
  },
  {
    kind: 'judge_verdict',
    round: 1,
    score: 0.52,
    passed: false,
    feedback: 'Add a source URL for the weather claim.',
    done: true,
    startedAt: 3_100,
    durationMs: 6_800,
    items: [{ kind: 'thinking', text: 'Weather claims are present but unsourced.' }],
  },
  { kind: 'revise', round: 1 },
  {
    kind: 'judge_verdict',
    round: 2,
    score: 0.88,
    passed: true,
    feedback: '',
    done: true,
    startedAt: 12_000,
    durationMs: 5_500,
    items: [{ kind: 'thinking', text: 'Sources added. Score: 0.88.' }],
  },
  { kind: 'text', text: 'Best time: **May–September**, per [Met Éireann](https://example.com).' },
]

// ---- stories ----------------------------------------------------------------

export const Queued: Story = {
  args: {
    node: wrNode,
    state: { status: 'queued' },
    parts: [],
    isFinal: false,
  },
}

// Running with a live node-header timer; use render so startedAt is computed at mount.
export const Running: Story = {
  render: () => (
    <DagNode
      node={wrNode}
      state={{ status: 'running', startedAt: Date.now() - 12_000 }}
      parts={[
        { kind: 'thinking', text: 'Searching for Dublin climate data…' },
        { kind: 'tool_call', name: 'web_search', args: { query: 'best time to visit Dublin weather' } },
      ]}
      isFinal={false}
    />
  ),
}

export const DoneWithTokens: Story = {
  args: {
    node: wrNode,
    state: {
      status: 'done',
      startedAt: 0,
      finishedAt: 34_000,
      totalTokens: 1_847,
      promptTokens: 1_200,
      completionTokens: 647,
      model: 'qwen3-30b-a3b',
      finishReason: 'STOP',
      serverDurationMs: 34_000,
    },
    parts: researchParts,
    isFinal: false,
  },
}

export const FinalNodeDone: Story = {
  args: {
    node: synthNode,
    state: {
      status: 'done',
      startedAt: 0,
      finishedAt: 22_500,
      totalTokens: 892,
      model: 'qwen3-30b-a3b',
    },
    parts: [
      {
        kind: 'text',
        text: '## Dublin Travel Guide\n\nVisit between **May and September** for the best weather.\n\n- Guinness Storehouse\n- Trinity College\n- Phoenix Park',
      },
    ],
    isFinal: true,
  },
}

export const WithSelfCritiqueDone: Story = {
  args: {
    node: wrNode,
    state: {
      status: 'done',
      startedAt: 0,
      finishedAt: 20_000,
      totalTokens: 1_203,
      model: 'qwen3-30b-a3b',
    },
    parts: withSelfCritParts,
    isFinal: false,
  },
}

// Self-critique actively running — its header timer should tick; the node timer should tick.
export const SelfCritiqueRunning: Story = {
  render: () => (
    <DagNode
      node={wrNode}
      state={{ status: 'running', startedAt: Date.now() - 8_000 }}
      parts={[
        { kind: 'tool_call', name: 'web_search', args: { query: 'Dublin weather' }, result: { results: [] } },
        {
          kind: 'self_refine',
          done: false,
          startedAt: Date.now() - 4_000,
          items: [{ kind: 'thinking', text: 'Reviewing my draft for citation gaps…' }],
        },
      ]}
      isFinal={false}
    />
  ),
}

// Self-critique done (frozen), judge now running — self-critique timer must not tick.
export const JudgeRunning: Story = {
  render: () => (
    <DagNode
      node={wrNode}
      state={{ status: 'running', startedAt: Date.now() - 15_000 }}
      parts={[
        { kind: 'tool_call', name: 'web_search', args: { query: 'Dublin weather' }, result: { results: [] } },
        {
          kind: 'self_refine',
          changed: false,
          done: true,
          startedAt: Date.now() - 12_000,
          durationMs: 3_000,
          items: [],
        },
        {
          kind: 'judge_verdict',
          round: 1,
          done: false,
          startedAt: Date.now() - 9_000,
          items: [{ kind: 'thinking', text: 'Evaluating the research output for factual accuracy…' }],
        },
      ]}
      isFinal={false}
    />
  ),
}

export const JudgeRoundsAllDone: Story = {
  args: {
    node: wrNode,
    state: {
      status: 'done',
      startedAt: 0,
      finishedAt: 62_000,
      totalTokens: 3_421,
      model: 'qwen3-30b-a3b',
    },
    parts: withJudgeRoundsParts,
    isFinal: false,
  },
}

export const Truncated: Story = {
  args: {
    node: wrNode,
    state: {
      status: 'done',
      startedAt: 0,
      finishedAt: 45_000,
      totalTokens: 8_192,
      model: 'qwen3-30b-a3b',
      finishReason: 'MAX_TOKENS',
    },
    parts: researchParts,
    isFinal: false,
  },
}

export const Failed: Story = {
  args: {
    node: wrNode,
    state: {
      status: 'failed',
      startedAt: 0,
      finishedAt: 5_000,
      error: 'web_fetch: connection timeout after 30s',
    },
    parts: [],
    isFinal: false,
  },
}
