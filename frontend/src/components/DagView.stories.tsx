import type { Meta, StoryObj } from '@storybook/react-vite'
import { DagView } from './DagView'
import type { DagTurnState } from '../state/chatStore'
import type { MessagePart } from './messageParts'

const meta: Meta<typeof DagView> = {
  title: 'Chat/DagView',
  component: DagView,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof DagView>

// ---- shared fixtures -------------------------------------------------------

const researchParts: MessagePart[] = [
  { kind: 'thinking', text: 'I need to find the best months to visit Dublin based on weather and tourism data.' },
  { kind: 'tool_call', name: 'web_search', args: { query: 'best time to visit Dublin weather' }, result: { results: [{ title: 'Dublin Climate Guide', url: 'https://example.com/dublin-climate' }] } },
  { kind: 'tool_call', name: 'web_fetch', args: { url: 'https://example.com/dublin-climate' }, result: 'Dublin has mild temperatures year-round. May–September is warmest (15–18 °C), with June–August the most popular months…' },
  { kind: 'text', text: 'The best months to visit Dublin are **May through September**, particularly June–August for warmest weather and longest daylight hours. ([Dublin Climate Guide](https://example.com/dublin-climate))' },
]

const activityParts: MessagePart[] = [
  { kind: 'thinking', text: 'Searching for top attractions and activities in Dublin.' },
  { kind: 'tool_call', name: 'web_search', args: { query: 'top things to do Dublin attractions' }, result: { results: [{ title: 'Visit Dublin', url: 'https://example.com/visit' }] } },
  { kind: 'tool_call', name: 'web_fetch', args: { url: 'https://example.com/visit' }, result: 'Top attractions: Guinness Storehouse, Trinity College, Phoenix Park, Temple Bar…' },
  { kind: 'text', text: 'Top things to do in Dublin: the **Guinness Storehouse**, **Trinity College Library** (Book of Kells), **Phoenix Park**, and the lively pubs of **Temple Bar**. ([Visit Dublin](https://example.com/visit))' },
]

const vettedResearchParts: MessagePart[] = [
  { kind: 'tool_call', name: 'web_search', args: { query: 'best time to visit Dublin weather' }, result: { results: [] } },
  {
    kind: 'judge_verdict',
    round: 1,
    score: 0.52,
    passed: false,
    feedback: 'The answer lacks a source URL for the weather claims.',
    done: true,
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
    items: [{ kind: 'thinking', text: 'Sources added. Score: 0.88.' }],
  },
  { kind: 'text', text: 'Best time to visit Dublin is **June–August**, per [Met Éireann](https://example.com/met).' },
]

const synthesisParts: MessagePart[] = [
  {
    kind: 'text',
    text: `## Best Time to Visit Dublin & What to Do

**When to go:** Visit between **May and September** for the best weather (15–18 °C). June–August has the longest daylight and the most events. Avoid November–February for rain and short days.

### Top Activities

- 🍺 **Guinness Storehouse** — a multi-floor museum ending with a pint and panoramic city views
- 📚 **Trinity College** — home of the Book of Kells and the stunning Long Room library
- 🌳 **Phoenix Park** — one of Europe's largest urban parks; spot the resident deer
- 🎶 **Temple Bar** — Dublin's cultural quarter with live trad music every night

*Sources: [Dublin Climate Guide](https://example.com/dublin-climate) · [Visit Dublin](https://example.com/visit)*`,
  },
]

// ---- helpers ---------------------------------------------------------------

function make3NodeDag(overrides: {
  r1?: Partial<{ status: DagTurnState['nodeStates'][string]['status']; error?: string; parts?: MessagePart[] }>
  r2?: Partial<{ status: DagTurnState['nodeStates'][string]['status']; error?: string; parts?: MessagePart[] }>
  synth?: Partial<{ status: DagTurnState['nodeStates'][string]['status']; error?: string; parts?: MessagePart[] }>
} = {}): DagTurnState {
  return {
    planId: 'plan-dublin-01',
    nodes: [
      { id: 'r1', agent: 'web-researcher', task: 'Find the best time to visit Dublin: climate, peak/off-peak seasons, and weather.', depends_on: [] },
      { id: 'r2', agent: 'web-researcher', task: 'Find the top things to do and see in Dublin: attractions, neighbourhoods, and hidden gems.', depends_on: [] },
      { id: 'synth', agent: 'synthesizer', task: 'Combine the research into a vetted itinerary guide.', depends_on: ['r1', 'r2'] },
    ],
    edges: [
      { from: 'r1', to: 'synth' },
      { from: 'r2', to: 'synth' },
    ],
    nodeStates: {
      r1: { status: overrides.r1?.status ?? 'queued', error: overrides.r1?.error },
      r2: { status: overrides.r2?.status ?? 'queued', error: overrides.r2?.error },
      synth: { status: overrides.synth?.status ?? 'queued', error: overrides.synth?.error },
    },
    nodeParts: {
      r1: overrides.r1?.parts ?? [],
      r2: overrides.r2?.parts ?? [],
      synth: overrides.synth?.parts ?? [],
    },
  }
}

// ---- stories ---------------------------------------------------------------

// Single-node DAG — a simple query that only needed one web-researcher.
export const SingleNodeDone: Story = {
  args: {
    dag: {
      planId: 'plan-single',
      nodes: [
        { id: 'r1', agent: 'web-researcher', task: 'Find the best time to visit Dublin.', depends_on: [] },
      ],
      edges: [],
      nodeStates: { r1: { status: 'done', outputPreview: 'Best time is May–September…' } },
      nodeParts: { r1: researchParts },
    } satisfies DagTurnState,
  },
}

// All three nodes just queued — the moment the dag_plan event arrives.
export const AllQueued: Story = {
  args: { dag: make3NodeDag() },
}

// Two researchers running in parallel, synthesizer still queued.
export const TwoResearchersRunning: Story = {
  args: {
    dag: make3NodeDag({
      r1: { status: 'running', parts: [
        { kind: 'thinking', text: 'Searching for Dublin climate data…' },
        { kind: 'tool_call', name: 'web_search', args: { query: 'best time to visit Dublin weather' } },
      ]},
      r2: { status: 'running', parts: [
        { kind: 'thinking', text: 'Looking up top Dublin attractions.' },
      ]},
    }),
  },
}

// One researcher done, one still running, synthesizer queued.
export const PartiallyDone: Story = {
  args: {
    dag: make3NodeDag({
      r1: { status: 'done', parts: researchParts },
      r2: { status: 'running', parts: [
        { kind: 'tool_call', name: 'web_search', args: { query: 'Dublin top attractions' }, result: { results: [{ title: 'Visit Dublin', url: 'https://example.com/visit' }] } },
        { kind: 'tool_call', name: 'web_fetch', args: { url: 'https://example.com/visit' } },
      ]},
    }),
  },
}

// Both researchers done, synthesizer streaming its final answer.
export const SynthesizerRunning: Story = {
  args: {
    dag: make3NodeDag({
      r1: { status: 'done', parts: researchParts },
      r2: { status: 'done', parts: activityParts },
      synth: { status: 'running', parts: [
        { kind: 'thinking', text: 'Combining both research streams into a structured guide…' },
        { kind: 'text', text: '## Best Time to Visit Dublin\n\nVisit between **May and September**…' },
      ]},
    }),
  },
}

// Fully completed DAG — the happy path for the "Dublin" done-when test.
export const FullyDone: Story = {
  args: {
    dag: make3NodeDag({
      r1: { status: 'done', parts: vettedResearchParts },
      r2: { status: 'done', parts: activityParts },
      synth: { status: 'done', parts: synthesisParts },
    }),
  },
}

// One researcher failed — synthesizer stays queued, error visible on failed node.
export const NodeFailed: Story = {
  args: {
    dag: make3NodeDag({
      r1: { status: 'done', parts: researchParts },
      r2: { status: 'failed', error: 'web_fetch: connection timeout after 30s' },
      synth: { status: 'queued' },
    }),
  },
}

// Researchers done with vetting rounds visible, synthesizer pending.
export const WithJudgeRounds: Story = {
  args: {
    dag: make3NodeDag({
      r1: { status: 'done', parts: vettedResearchParts },
      r2: { status: 'running', parts: [
        { kind: 'tool_call', name: 'web_search', args: { query: 'Dublin attractions' }, result: { results: [] } },
        {
          kind: 'judge_verdict',
          round: 1,
          score: 0.61,
          passed: false,
          feedback: 'List attractions with context, not just names.',
          done: true,
          items: [{ kind: 'thinking', text: 'Attractions are listed without descriptions. Needs revision.' }],
        },
        { kind: 'revise', round: 1 },
      ]},
    }),
  },
}

// Fully done with per-node timing and token metadata — shows token total in footer.
export const FullyDoneWithMetadata: Story = {
  args: {
    dag: {
      planId: 'plan-dublin-01',
      nodes: [
        { id: 'r1', agent: 'web-researcher', task: 'Find the best time to visit Dublin: climate, peak/off-peak seasons, and weather.', depends_on: [] },
        { id: 'r2', agent: 'web-researcher', task: 'Find the top things to do and see in Dublin: attractions, neighbourhoods, and hidden gems.', depends_on: [] },
        { id: 'synth', agent: 'synthesizer', task: 'Combine the research into a vetted itinerary guide.', depends_on: ['r1', 'r2'] },
      ],
      edges: [{ from: 'r1', to: 'synth' }, { from: 'r2', to: 'synth' }],
      nodeStates: {
        r1: { status: 'done', startedAt: 0, finishedAt: 34_000, totalTokens: 1_847, promptTokens: 1_200, completionTokens: 647, model: 'qwen3-30b-a3b', finishReason: 'STOP', serverDurationMs: 34_000 },
        r2: { status: 'done', startedAt: 0, finishedAt: 28_500, totalTokens: 1_603, promptTokens: 980, completionTokens: 623, model: 'qwen3-30b-a3b', finishReason: 'STOP', serverDurationMs: 28_500 },
        synth: { status: 'done', startedAt: 35_000, finishedAt: 57_000, totalTokens: 892, promptTokens: 650, completionTokens: 242, model: 'qwen3-30b-a3b', finishReason: 'STOP', serverDurationMs: 22_000 },
      },
      nodeParts: {
        r1: vettedResearchParts,
        r2: activityParts,
        synth: synthesisParts,
      },
      startedAt: 0,
      finishedAt: 57_000,
    } satisfies DagTurnState,
  },
}

// Two researchers running in parallel with live timers — use render so startedAt is relative to mount.
export const LiveParallelRun: Story = {
  render: () => {
    const now = Date.now()
    return (
      <DagView
        dag={{
          planId: 'plan-live',
          nodes: [
            { id: 'r1', agent: 'web-researcher', task: 'Find the best time to visit Dublin: climate, peak/off-peak seasons, and weather.', depends_on: [] },
            { id: 'r2', agent: 'web-researcher', task: 'Find the top things to do and see in Dublin: attractions, neighbourhoods, and hidden gems.', depends_on: [] },
            { id: 'synth', agent: 'synthesizer', task: 'Combine the research into a vetted itinerary guide.', depends_on: ['r1', 'r2'] },
          ],
          edges: [{ from: 'r1', to: 'synth' }, { from: 'r2', to: 'synth' }],
          nodeStates: {
            r1: { status: 'running', startedAt: now - 8_000, outputChars: 320 },
            r2: { status: 'running', startedAt: now - 8_000, outputChars: 140 },
            synth: { status: 'queued' },
          },
          nodeParts: {
            r1: [
              { kind: 'thinking', text: 'Searching for Dublin climate data…' },
              { kind: 'tool_call', name: 'web_search', args: { query: 'Dublin weather best months' }, result: { results: [{ title: 'Dublin Climate Guide', url: 'https://example.com' }] } },
              { kind: 'tool_call', name: 'web_fetch', args: { url: 'https://example.com' } },
            ],
            r2: [
              { kind: 'thinking', text: 'Looking up top Dublin attractions.' },
              { kind: 'tool_call', name: 'web_search', args: { query: 'top things to do Dublin' } },
            ],
            synth: [],
          },
          startedAt: now - 8_000,
        }}
      />
    )
  },
}
