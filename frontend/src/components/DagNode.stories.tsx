import type { Meta, StoryObj } from '@storybook/react-vite'
import { DagNode } from './DagNode'
import type { DagNodeDef } from '../state/agentStream'
import type { AgentRun, Activity } from './messageParts'

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

// ---- run fixtures -----------------------------------------------------------

const researchActivity: Activity[] = [
  { kind: 'thinking', text: 'I need the best months to visit Dublin based on weather data.' },
  { kind: 'tool', tool: { callId: 'c1', name: 'web_search', args: { query: 'best time to visit Dublin weather' }, result: { results: [{ title: 'Dublin Climate Guide', url: 'https://example.com/climate' }] }, done: true } },
  { kind: 'tool', tool: { callId: 'c2', name: 'web_fetch', args: { url: 'https://example.com/climate' }, result: 'Dublin is mild year-round; May–September is warmest (15–18 °C).', done: true } },
]

const workerDone = (activity: Activity[]): AgentRun => ({
  runId: 'r1', agent: 'web-researcher', stage: 'worker', activity, done: true, finishReason: 'STOP',
})

const judgeRun = (round: number, score: number, passed: boolean, feedback: string): AgentRun => ({
  runId: `j${round}`, agent: 'judge', stage: 'judge', round, done: true, score, passed, feedback,
  activity: [{ kind: 'thinking', text: 'Re-checking cited URLs against the claims…' }],
})

// ---- stories ----------------------------------------------------------------

export const Queued: Story = {
  args: { node: wrNode, state: { status: 'queued' }, runs: [], answer: '', isFinal: false },
}

export const Running: Story = {
  render: () => (
    <DagNode
      node={wrNode}
      state={{ status: 'running', startedAt: Date.now() - 12_000 }}
      runs={[{
        runId: 'r1', agent: 'web-researcher', stage: 'worker', done: false,
        activity: [
          { kind: 'thinking', text: 'Searching for Dublin climate data…' },
          { kind: 'tool', tool: { callId: 'c1', name: 'web_search', args: { query: 'best time to visit Dublin weather' }, done: false } },
        ],
      }]}
      answer=""
      isFinal={false}
    />
  ),
}

export const DoneWithTokens: Story = {
  args: {
    node: wrNode,
    state: { status: 'done', startedAt: 0, finishedAt: 34_000, totalTokens: 1_847, model: 'qwen3-30b-a3b', finishReason: 'STOP' },
    runs: [workerDone(researchActivity)],
    answer: 'Best months to visit Dublin: **May–September**, warmest June–August.',
    isFinal: false,
  },
}

export const FinalNodeDone: Story = {
  args: {
    node: synthNode,
    state: { status: 'done', startedAt: 0, finishedAt: 22_500, totalTokens: 892, model: 'qwen3-30b-a3b' },
    runs: [workerDone([{ kind: 'thinking', text: 'Combining the research into a guide.' }])],
    answer: '## Dublin Travel Guide\n\nVisit between **May and September** for the best weather.\n\n- Guinness Storehouse\n- Trinity College\n- Phoenix Park',
    isFinal: true,
  },
}

export const SelfCritiqueRunning: Story = {
  render: () => (
    <DagNode
      node={wrNode}
      state={{ status: 'running', startedAt: Date.now() - 8_000 }}
      runs={[
        workerDone(researchActivity),
        { runId: 'sr', agent: 'web-researcher', stage: 'self_refine', done: false, activity: [{ kind: 'thinking', text: 'Reviewing my draft for citation gaps…' }] },
      ]}
      answer=""
      isFinal={false}
    />
  ),
}

export const JudgeRunning: Story = {
  render: () => (
    <DagNode
      node={wrNode}
      state={{ status: 'running', startedAt: Date.now() - 15_000 }}
      runs={[
        workerDone(researchActivity),
        { runId: 'j1', agent: 'judge', stage: 'judge', round: 1, done: false, activity: [{ kind: 'thinking', text: 'Independently re-fetching the cited URLs…' }, { kind: 'tool', tool: { callId: 'jc1', name: 'web_fetch', args: { url: 'https://example.com/climate' }, done: false } }] },
      ]}
      answer=""
      isFinal={false}
    />
  ),
}

export const JudgeRoundsAllDone: Story = {
  args: {
    node: wrNode,
    state: { status: 'done', startedAt: 0, finishedAt: 62_000, totalTokens: 3_421, model: 'qwen3-30b-a3b' },
    runs: [
      workerDone(researchActivity),
      { runId: 'sr', agent: 'web-researcher', stage: 'self_refine', done: true, changed: false, activity: [] },
      judgeRun(1, 0.52, false, 'Add a source URL for the weather claim.'),
      { runId: 'rev1', agent: 'web-researcher', stage: 'revise', round: 1, done: true, activity: [{ kind: 'tool', tool: { callId: 'rc1', name: 'web_fetch', args: { url: 'https://example.com/met' }, result: 'Met Éireann climate averages…', done: true } }] },
      judgeRun(2, 0.88, true, ''),
    ],
    answer: 'Best time: **May–September**, per [Met Éireann](https://example.com).',
    isFinal: false,
  },
}

export const JudgeUnavailable: Story = {
  args: {
    node: wrNode,
    state: { status: 'done', startedAt: 0, finishedAt: 30_000, model: 'qwen3-30b-a3b', judgeRounds: 1, judgePassed: false },
    runs: [
      workerDone(researchActivity),
      { runId: 'j1', agent: 'judge', stage: 'judge', round: 1, done: true, status: 'unavailable', reason: 'judge model timeout', activity: [] },
    ],
    answer: 'Best time: **May–September**.',
    isFinal: false,
  },
}

export const Truncated: Story = {
  args: {
    node: wrNode,
    state: { status: 'done', startedAt: 0, finishedAt: 45_000, totalTokens: 8_192, model: 'qwen3-30b-a3b', finishReason: 'MAX_TOKENS' },
    runs: [workerDone(researchActivity)],
    answer: 'Best months to visit Dublin: **May–September**.',
    isFinal: false,
  },
}

export const Failed: Story = {
  args: {
    node: wrNode,
    state: { status: 'failed', startedAt: 0, finishedAt: 5_000, error: 'web_fetch: connection timeout after 30s' },
    runs: [],
    answer: '',
    isFinal: false,
  },
}
