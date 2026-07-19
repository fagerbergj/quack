import type { Meta, StoryObj } from '@storybook/react-vite'
import { within, userEvent } from 'storybook/test'
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
  args: {
    node: wrNode, state: { status: 'queued' }, runs: [], answer: '', isFinal: false,
    onCancel: () => {}, onEditTask: () => {},
  },
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
      onCancel={() => {}}
      onPause={() => {}}
      onQueueMessage={() => {}}
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

// Every stage card shows the model that produced it (RunModel, next to the timer) —
// each stage can run a different model (e.g. a cheaper judge model).
export const JudgeRoundsAllDone: Story = {
  args: {
    node: wrNode,
    state: { status: 'done', startedAt: 0, finishedAt: 62_000, totalTokens: 3_421, model: 'qwen3-30b-a3b' },
    runs: [
      { ...workerDone(researchActivity), model: 'qwen3-30b-a3b' },
      { ...judgeRun(1, 0.52, false, 'Add a source URL for the weather claim.'), model: 'gemma3-27b' },
      { runId: 'rev1', agent: 'web-researcher', stage: 'revise', round: 1, done: true, model: 'qwen3-30b-a3b', activity: [{ kind: 'tool', tool: { callId: 'rc1', name: 'web_fetch', args: { url: 'https://example.com/met' }, result: 'Met Éireann climate averages…', done: true } }] },
      { ...judgeRun(2, 0.88, true, ''), model: 'gemma3-27b' },
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

// #379: a node whose worker run streamed 80 tool-call events — the
// performance case the streaming-update fix targets (see messageParts.ts /
// AgentParts.test.ts). Renders via the same WorkerCard + ActivityList path
// as a live run; ActivityList windows to its most recent items so this stays
// cheap however many events arrive.
const manyToolActivity: Activity[] = Array.from({ length: 80 }, (_, i) => ({
  kind: 'tool' as const,
  tool: { callId: `c${i}`, name: 'web_search', args: { query: `query ${i}` }, result: { results: [] }, done: true },
}))

export const ManyToolCallsStreaming: Story = {
  render: () => (
    <DagNode
      node={wrNode}
      state={{ status: 'running', startedAt: Date.now() - 45_000 }}
      runs={[{ runId: 'r1', agent: 'web-researcher', stage: 'worker', done: false, activity: manyToolActivity }]}
      answer=""
      isFinal={false}
    />
  ),
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

// ---- #265: pause / cancel / queued-message states ---------------------------

export const Cancelled: Story = {
  args: {
    node: wrNode,
    state: { status: 'cancelled', startedAt: 0, finishedAt: 8_000 },
    runs: [workerDone(researchActivity.slice(0, 1))],
    answer: '',
    isFinal: false,
  },
}

export const Paused: Story = {
  render: () => (
    <DagNode
      node={wrNode}
      state={{ status: 'paused', startedAt: Date.now() - 20_000 }}
      runs={[workerDone(researchActivity)]}
      answer="Best months to visit Dublin so far: **May–September** (draft, paused before the judge round)."
      isFinal={false}
      onResume={() => {}}
      onCancel={() => {}}
    />
  ),
}

// A running node with a queued (not-yet-delivered) message showing the ✉
// badge — the message itself is edited/removed in the popup (#384).
export const RunningWithQueuedMessage: Story = {
  render: () => (
    <DagNode
      node={wrNode}
      state={{
        status: 'running', startedAt: Date.now() - 9_000,
        queue: [{ id: 'q1', text: 'Also check winter rainfall.', delivered: false, created_at: new Date().toISOString() }],
      }}
      runs={[{ runId: 'r1', agent: 'web-researcher', stage: 'worker', done: false, activity: researchActivity }]}
      answer=""
      isFinal={false}
      onCancel={() => {}}
      onPause={() => {}}
      onQueueMessage={() => {}}
    />
  ),
}

// Demonstrates Feature 1 (height-lock) in context: a long answer is capped with a
// Show more toggle, and a many-round node stays scannable because each round is a
// collapsed card. Open the Work card to see the edit_file diff render (Feature 2).
const codeNode: DagNodeDef = {
  id: 'code', agent: 'web-researcher',
  task: 'Refactor greet() to support a loud flag and add a test.', depends_on: [],
}
const longAnswer = ['# Refactor complete', '', ...Array.from({ length: 24 }, (_, i) =>
  `${i + 1}. Applied change ${i + 1}: adjusted the greeting builder and covered it with a case.`)].join('\n')

export const LongContentManyRounds: Story = {
  args: {
    node: codeNode,
    state: { status: 'done', startedAt: 0, finishedAt: 88_000, totalTokens: 6_120, model: 'qwen3-30b-a3b' },
    runs: [
      { runId: 'w', agent: 'web-researcher', stage: 'worker', done: true, model: 'qwen3-30b-a3b', activity: [
        { kind: 'thinking', text: 'I will change the signature and update the body, then run the tests.' },
        { kind: 'tool', tool: { callId: 't1', name: 'edit_file', done: true,
          args: { path: 'internal/greet/greet.go', old: 'func greet(name string) string {\n\treturn "Hello, " + name\n}', new: 'func greet(name string, loud bool) string {\n\tg := "Hello, " + name\n\tif loud {\n\t\tg += "!"\n\t}\n\treturn g\n}' },
          result: { replacements: 1 } } },
        { kind: 'tool', tool: { callId: 't2', name: 'run_command', done: true,
          args: { dir: '.', command: 'go test ./...' }, result: { exit_code: 0, output: 'ok\tgithub.com/fagerbergj/quack/internal/greet\t0.11s', duration_ms: 130 } } },
      ] },
      judgeRun(1, 0.48, false, 'The loud path is untested. Add a case asserting the "!" suffix.'),
      { runId: 'rev1', agent: 'web-researcher', stage: 'revise', round: 1, done: true, model: 'qwen3-30b-a3b', activity: [
        { kind: 'tool', tool: { callId: 't3', name: 'write_file', done: true, args: { path: 'internal/greet/greet_test.go', content: 'package greet\n\nimport "testing"\n\nfunc TestLoud(t *testing.T) { /* … */ }' }, result: { created: true } } },
      ] },
      judgeRun(2, 0.6, false, 'Closer, but assert the exact output.'),
      { runId: 'rev2', agent: 'web-researcher', stage: 'revise', round: 2, done: true, model: 'qwen3-30b-a3b', activity: [] },
      judgeRun(3, 0.86, true, ''),
    ],
    answer: longAnswer,
    isFinal: true,
  },
}

// ---- 0.9.0: ⋮ overflow menu + needs_input (#384/#265 follow-up) ------------

// A mid-node HITL question (StatusDot amber, matching needs_input everywhere
// else in the app) — answering it happens in the popup (menu → "Answer
// question…"), not here.
export const NeedsInput: Story = {
  args: {
    node: wrNode,
    state: { status: 'needs_input', question: 'Should I include hostel prices, or hotels only?', startedAt: Date.now() - 6_000 },
    runs: [{ runId: 'r1', agent: 'web-researcher', stage: 'worker', done: false, activity: researchActivity.slice(0, 1) }],
    answer: '',
    isFinal: false,
    onCancel: () => {},
    onAnswerQuestion: () => {},
  },
}

// The ⋮ menu opened on a running node: Pause + Cancel one click away, plus
// "Queue a message…" (opens the popup — it needs the input).
export const OverflowMenuRunning: Story = {
  render: () => (
    <DagNode
      node={wrNode}
      state={{ status: 'running', startedAt: Date.now() - 12_000 }}
      runs={[{ runId: 'r1', agent: 'web-researcher', stage: 'worker', done: false, activity: researchActivity.slice(0, 1) }]}
      answer=""
      isFinal={false}
      onCancel={() => {}}
      onPause={() => {}}
      onQueueMessage={() => {}}
    />
  ),
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Node actions' }))
  },
}

// The ⋮ menu opened on a paused node: Resume + Cancel.
export const OverflowMenuPaused: Story = {
  render: () => (
    <DagNode
      node={wrNode}
      state={{ status: 'paused', startedAt: Date.now() - 20_000 }}
      runs={[workerDone(researchActivity)]}
      answer=""
      isFinal={false}
      onResume={() => {}}
      onCancel={() => {}}
    />
  ),
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Node actions' }))
  },
}

// The ⋮ menu opened on a needs_input node: Cancel + "Answer question…".
export const OverflowMenuNeedsInput: Story = {
  render: () => (
    <DagNode
      node={wrNode}
      state={{ status: 'needs_input', question: 'Should I include hostel prices, or hotels only?', startedAt: Date.now() - 6_000 }}
      runs={[{ runId: 'r1', agent: 'web-researcher', stage: 'worker', done: false, activity: researchActivity.slice(0, 1) }]}
      answer=""
      isFinal={false}
      onCancel={() => {}}
      onAnswerQuestion={() => {}}
    />
  ),
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement)
    await userEvent.click(canvas.getByRole('button', { name: 'Node actions' }))
  },
}

// A terminal node (done/failed/cancelled) has no ⋮ menu at all — nothing left
// to do. Regression guard for the menu's terminal-state hiding.
export const OverflowMenuHiddenOnTerminal: Story = {
  args: {
    node: wrNode,
    state: { status: 'done', startedAt: 0, finishedAt: 10_000 },
    runs: [workerDone(researchActivity.slice(0, 1))],
    answer: 'Best months to visit Dublin: **May–September**.',
    isFinal: false,
  },
}
