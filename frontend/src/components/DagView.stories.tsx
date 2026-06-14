import type { Meta, StoryObj } from '@storybook/react-vite'
import { DagView } from './DagView'
import type { DagTurnState } from '../state/chatStore'
import type { AgentRun, Activity } from './messageParts'

const meta: Meta<typeof DagView> = {
  title: 'Chat/DagView',
  component: DagView,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof DagView>

// ---- run fixtures ----------------------------------------------------------

const climateActivity: Activity[] = [
  { kind: 'thinking', text: 'Searching for Dublin climate data…' },
  { kind: 'tool', tool: { callId: 'c1', name: 'web_search', args: { query: 'best time to visit Dublin weather' }, result: { results: [{ title: 'Dublin Climate Guide', url: 'https://example.com/climate' }] }, done: true } },
  { kind: 'tool', tool: { callId: 'c2', name: 'web_fetch', args: { url: 'https://example.com/climate' }, result: 'Dublin is mild year-round; May–September warmest.', done: true } },
]

const attractionsActivity: Activity[] = [
  { kind: 'thinking', text: 'Looking up top Dublin attractions.' },
  { kind: 'tool', tool: { callId: 'c3', name: 'web_search', args: { query: 'top things to do Dublin' }, result: { results: [{ title: 'Visit Dublin', url: 'https://example.com/visit' }] }, done: true } },
]

const worker = (id: string, activity: Activity[], done: boolean): AgentRun =>
  ({ runId: `${id}-w`, agent: 'web-researcher', stage: 'worker', activity, done, finishReason: done ? 'STOP' : undefined })

const judge = (id: string, round: number, score: number, passed: boolean, feedback: string): AgentRun =>
  ({ runId: `${id}-j${round}`, agent: 'judge', stage: 'judge', round, done: true, score, passed, feedback, activity: [{ kind: 'thinking', text: 'Re-fetching cited URLs to verify the claims…' }] })

const climateAnswer = 'Best months to visit Dublin: **May–September**, per [Met Éireann](https://example.com/met).'
const attractionsAnswer = 'Top things to do: **Guinness Storehouse**, **Trinity College**, **Phoenix Park**, **Temple Bar**.'
const synthAnswer = '## Dublin Guide\n\nVisit **May–September**. Don\'t miss the Guinness Storehouse, Trinity College, and Phoenix Park.'

function dag(over: {
  r1?: { status: DagTurnState['nodeStates'][string]['status']; error?: string; runs?: AgentRun[]; answer?: string }
  r2?: { status: DagTurnState['nodeStates'][string]['status']; error?: string; runs?: AgentRun[]; answer?: string }
  synth?: { status: DagTurnState['nodeStates'][string]['status']; error?: string; runs?: AgentRun[]; answer?: string }
} = {}): DagTurnState {
  return {
    planId: 'plan-dublin-01',
    nodes: [
      { id: 'r1', agent: 'web-researcher', task: 'Find the best time to visit Dublin: climate and seasons.', depends_on: [] },
      { id: 'r2', agent: 'web-researcher', task: 'Find the top things to do and see in Dublin.', depends_on: [] },
      { id: 'synth', agent: 'synthesizer', task: 'Combine the research into a vetted itinerary guide.', depends_on: ['r1', 'r2'] },
    ],
    edges: [{ from: 'r1', to: 'synth' }, { from: 'r2', to: 'synth' }],
    nodeStates: {
      r1: { status: over.r1?.status ?? 'queued', error: over.r1?.error },
      r2: { status: over.r2?.status ?? 'queued', error: over.r2?.error },
      synth: { status: over.synth?.status ?? 'queued', error: over.synth?.error },
    },
    nodeRuns: {
      r1: over.r1?.runs ?? [],
      r2: over.r2?.runs ?? [],
      synth: over.synth?.runs ?? [],
    },
    nodeAnswer: {
      r1: over.r1?.answer ?? '',
      r2: over.r2?.answer ?? '',
      synth: over.synth?.answer ?? '',
    },
  }
}

// ---- stories ---------------------------------------------------------------

export const AllQueued: Story = { args: { dag: dag() } }

export const TwoResearchersRunning: Story = {
  args: {
    dag: dag({
      r1: { status: 'running', runs: [worker('r1', climateActivity, false)] },
      r2: { status: 'running', runs: [worker('r2', attractionsActivity, false)] },
    }),
  },
}

export const PartiallyDone: Story = {
  args: {
    dag: dag({
      r1: { status: 'done', runs: [worker('r1', climateActivity, true)], answer: climateAnswer },
      r2: { status: 'running', runs: [worker('r2', attractionsActivity, false)] },
    }),
  },
}

export const WithJudgeRounds: Story = {
  args: {
    dag: dag({
      r1: {
        status: 'done', answer: climateAnswer, runs: [
          worker('r1', climateActivity, true),
          judge('r1', 1, 0.52, false, 'Add a source URL for the weather claim.'),
          { runId: 'r1-rev1', agent: 'web-researcher', stage: 'revise', round: 1, done: true, activity: [{ kind: 'tool', tool: { callId: 'rc1', name: 'web_fetch', args: { url: 'https://example.com/met' }, result: 'Met Éireann averages…', done: true } }] },
          judge('r1', 2, 0.88, true, ''),
        ],
      },
      r2: { status: 'running', runs: [worker('r2', attractionsActivity, false)] },
    }),
  },
}

export const NodeFailed: Story = {
  args: {
    dag: dag({
      r1: { status: 'done', runs: [worker('r1', climateActivity, true)], answer: climateAnswer },
      r2: { status: 'failed', error: 'web_fetch: connection timeout after 30s' },
    }),
  },
}

export const FullyDone: Story = {
  args: {
    dag: {
      ...dag({
        r1: { status: 'done', runs: [worker('r1', climateActivity, true), judge('r1', 1, 0.88, true, '')], answer: climateAnswer },
        r2: { status: 'done', runs: [worker('r2', attractionsActivity, true)], answer: attractionsAnswer },
        synth: { status: 'done', runs: [worker('synth', [{ kind: 'thinking', text: 'Combining both research streams…' }], true)], answer: synthAnswer },
      }),
      nodeStates: {
        r1: { status: 'done', startedAt: 0, finishedAt: 34_000, totalTokens: 1_847, model: 'qwen3-30b-a3b' },
        r2: { status: 'done', startedAt: 0, finishedAt: 28_500, totalTokens: 1_603, model: 'qwen3-30b-a3b' },
        synth: { status: 'done', startedAt: 35_000, finishedAt: 57_000, totalTokens: 892, model: 'qwen3-30b-a3b' },
      },
      startedAt: 0,
      finishedAt: 57_000,
    },
  },
}
