import type { Meta, StoryObj } from '@storybook/react-vite'
import { JudgeRoundBreakdown, NodeQualityCheck } from './NodeQualityCheck'
import { parseJudgeRound } from '../lib/judgeRounds'

const meta: Meta<typeof NodeQualityCheck> = {
  title: 'Chat/NodeQualityCheck',
  component: NodeQualityCheck,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof NodeQualityCheck>

// Shaped like QA's Zig-vs-Odin synthesizer: three rounds, each sunk by the deterministic cites_sources probe.
const round = (n: number, score: number) => ({
  turn: 'e-6e4b3898', round: n, passed: false, score,
  criteria: [
    { name: 'citation_quality', score: n === 1 ? 0.67 : 1, feedback: "The Odin Philosophy cell attributes the quote to the 2021 post; it is from the 2018 essay." },
    { name: 'cites_sources', score, feedback: 'deterministic: 17 cited link(s), mean backing 0.28 - not fully backed: https://odin-lang.org/docs/faq/ (0.00), https://pkg.odin-lang.org/core/os/ (0.00)' },
    { name: 'clean_output', score: 1, feedback: 'Opens with the table title, no preamble.' },
    { name: 'grounded', score: 1 },
  ],
})
const bodies: Record<string, unknown> = {
  'judge_round:e-6e4b3898-synth-1-1': round(1, 0.28),
  'judge_round:e-6e4b3898-synth-1-2': round(2, 0.28),
  'judge_round:e-6e4b3898-synth-1-3': { ...round(3, 0.28), criteria: [{ name: 'cites_sources', score: 0.28 }, { name: 'specifics_cited', score: 1 }] },
}
const START = Date.parse('2026-10-12T18:30:00Z')
const list = {
  data: Object.keys(bodies).map((name, i) => ({
    name, kind: 'judge_round', class: 'structured', latest_revision: 1, revisions: [],
    lineage: { node_id: 'synth-1', round: i + 1, author: 'judge', saved_at: new Date(START + (i + 1) * 60_000).toISOString() },
  })),
}

// Routes only this story's chat and hands the rest to whatever fetch (or other story's stub) was there.
const withFetch = (Story: () => React.JSX.Element) => {
  const prev = window.fetch
  if (!('qcStub' in prev)) {
    window.fetch = Object.assign(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = decodeURIComponent(input instanceof Request ? input.url : String(input))
      if (url.endsWith('/chats/qc-chat/artifacts')) return new Response(JSON.stringify(list), { headers: { 'Content-Type': 'application/json' } })
      const name = Object.keys(bodies).find(n => url.includes(`/artifacts/${n}`))
      return name ? new Response(JSON.stringify(bodies[name])) : prev(input, init)
    }, { qcStub: true })
  }
  return <Story />
}

const props = { chatId: 'qc-chat', nodeId: 'synth-1', startedAt: START, finishedAt: START + 10 * 60_000 }

// Opened: every round's criteria, with the criterion that sank each failed round named on its line.
export const Opened: Story = {
  decorators: [withFetch],
  render: () => <NodeQualityCheck {...props} />,
  play: ({ canvasElement }) => {
    const d = canvasElement.querySelector('details')
    if (d) d.open = true
  },
}

export const FailedRound: StoryObj<typeof JudgeRoundBreakdown> = {
  render: () => <JudgeRoundBreakdown detail={parseJudgeRound(round(1, 0.57), 1)!} />,
}

export const PassedRound: StoryObj<typeof JudgeRoundBreakdown> = {
  render: () => <JudgeRoundBreakdown detail={parseJudgeRound({ round: 2, passed: true, score: 0.96, criteria: [{ name: 'specifics_cited', score: 0.96 }, { name: 'clean_output', score: 1 }] }, 2)!} />,
}
