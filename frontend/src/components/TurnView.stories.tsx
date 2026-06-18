import type { Meta, StoryObj } from '@storybook/react-vite'
import { TurnView } from './TurnView'
import { Dots } from './AgentParts'
import type { Turn } from '../generated'

function turn(content: string, ...output: Turn['output']): Turn {
  return { id: 't1', created_at: '', input: { role: 'user', content }, output }
}

const ANSWER = turn(
  'Best time to visit Dublin?',
  {
    type: 'message', id: 'm1', status: 'completed',
    content: [{ type: 'output_text', text: '**May–September** is warmest (15–18 °C) and driest. Avoid November–January for rain and short days.' }],
  },
)

const RESEARCH = turn(
  'Plan a 3-day Dublin trip',
  {
    type: 'quack:dag', id: 'd1', status: 'completed', plan_id: 'p1',
    nodes: [
      { id: 'n1', agent: 'web-researcher', task: 'Best time to visit', depends_on: [] },
      { id: 'n2', agent: 'web-researcher', task: 'Things to do', depends_on: [] },
      { id: 'n3', agent: 'synthesizer', task: 'Compose itinerary', depends_on: ['n1', 'n2'] },
    ],
    edges: [{ from: 'n1', to: 'n3' }, { from: 'n2', to: 'n3' }],
    node_states: {
      n1: { status: 'done', model: 'gpt-oss-120b', total_tokens: 1840, judge_passed: true, judge_final_score: 0.88 },
      n2: { status: 'done', model: 'gpt-oss-120b', total_tokens: 2100, judge_passed: true, judge_final_score: 0.81 },
      n3: { status: 'done', model: 'gpt-oss-120b', total_tokens: 1320, judge_passed: true, judge_final_score: 0.9 },
    },
  },
  {
    type: 'message', id: 'm2', status: 'completed',
    content: [{ type: 'output_text', text: '### Day 1\nTrinity College & the Book of Kells…' }],
  },
)

const meta: Meta<typeof TurnView> = {
  title: 'Chat/TurnView',
  component: TurnView,
  args: {
    idx: 0,
    isChoiceAnswer: false,
    submittingChoice: false,
    isCopied: false,
    onChoice: () => {},
    onCopy: () => alert('copied'),
    onDownload: () => alert('download'),
  },
  decorators: [Story => <div className="max-w-3xl mx-auto p-6 bg-gray-50 dark:bg-gray-900"><Story /></div>],
}
export default meta

type Story = StoryObj<typeof TurnView>

// A plain Q&A turn: user bubble + a markdown answer with Copy/Download.
export const Basic: Story = { args: { turn: ANSWER } }

// A research turn: collapsed "Research steps" DAG above the synthesized answer.
export const WithResearch: Story = { args: { turn: RESEARCH } }

// The pending indicator shown the instant a follow-up is submitted, before the first
// token streams in (§0b). Rendered standalone here since it lives inline in Chat.
export const Submitting: StoryObj = {
  render: () => (
    <div>
      <div className="flex justify-end mb-3">
        <div className="max-w-2xl ml-auto">
          <div className="bg-blue-600 text-white rounded-2xl rounded-tr-sm px-4 py-3 text-sm">
            What about restaurants?
          </div>
        </div>
      </div>
      <div className="flex justify-start">
        <div className="w-auto">
          <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-2xl rounded-tl-sm px-5 py-4" role="status" aria-label="Thinking">
            <Dots className="h-5" size="w-2 h-2" />
          </div>
        </div>
      </div>
    </div>
  ),
}
