// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { TurnView } from './TurnView'
import { ChatStoreProvider } from '../state/ChatStoreProvider'
import type { Turn } from '../generated'

afterEach(cleanup)

// Two researchers and no synthesizer: the reloaded turn's answer is one labelled section per sink.
const multiSinkTurn = {
  id: 't1',
  created_at: '2026-01-01T00:00:00Z',
  input: { role: 'user', content: 'exactly two researchers, no synthesizer' },
  output: [
    {
      type: 'quack:dag', id: 'd1', status: 'completed', plan_id: 'p1',
      nodes: [
        { id: 'r1', agent: 'web-researcher', task: 'a', depends_on: [] },
        { id: 'r2', agent: 'web-researcher', task: 'b', depends_on: [] },
      ],
      edges: [],
      node_states: { r1: { status: 'done' }, r2: { status: 'done' } },
    },
    { type: 'message', id: 'm1', status: 'completed', content: [{ type: 'output_text', text: '## r1\n\nONE\n\n## r2\n\nTWO' }] },
  ],
} as unknown as Turn

describe('TurnView multi-sink answer', () => {
  it('renders every sink as its own section in one answer bubble', () => {
    render(
      <ChatStoreProvider>
        <TurnView turn={multiSinkTurn} idx={0} isChoiceAnswer={false} submittingChoice={false} isCopied={false}
          priorContents={[]} onChoice={() => {}} onCopy={() => {}} onDownload={() => {}} />
      </ChatStoreProvider>,
    )
    expect(screen.getByRole('heading', { name: 'r1' })).toBeTruthy()
    expect(screen.getByRole('heading', { name: 'r2' })).toBeTruthy()
    expect(screen.getByText('ONE')).toBeTruthy()
    expect(screen.getByText('TWO')).toBeTruthy()
  })
})
