import type { Meta, StoryObj } from '@storybook/react-vite'
import { NodePopup } from './NodePopup'
import type { DagNodeDef } from '../state/agentStream'

const meta: Meta<typeof NodePopup> = {
  title: 'Chat/NodePopup',
  component: NodePopup,
  // Bottom sheet below 600px: every story renders at 390px in render-check.
  parameters: { layout: 'fullscreen', renderCheck: { viewports: ['mobile', 'desktop'] } },
}
export default meta

type Story = StoryObj<typeof NodePopup>

const node: DagNodeDef = {
  id: 'r1',
  agent: 'web-researcher',
  task: '## Task\n\nFind the best time to visit Dublin: climate, peak/off-peak seasons, and rainfall data.\n\n- Cite sources\n- Prefer official meteorological data',
  depends_on: [],
}

// The prompt renders through the same BubbleHeader + AssistantText markdown
// treatment as a chat bubble - no controls (a completed/historical turn).
export const ReadOnly: Story = {
  args: {
    node,
    state: { status: 'done' },
    onClose: () => {},
  },
}

// A not-yet-started node: the prompt is editable. Pause/cancel live in DagNode's ⋮ menu.
export const PendingEditablePrompt: Story = {
  args: {
    node,
    state: { status: 'queued' },
    onClose: () => {},
    onEditTask: () => {},
  },
}

// A running node: the queue input + history, one queued message pending.
export const RunningWithQueue: Story = {
  args: {
    node,
    state: {
      status: 'running',
      queue: [
        { id: 'q1', text: 'Also check winter rainfall.', status: 'queued', delivered: false, created_at: new Date().toISOString() },
        { id: 'q0', text: 'Use the last 10 years of data.', status: 'drained', delivered: true, created_at: new Date().toISOString() },
      ],
    },
    onClose: () => {},
    onQueueMessage: () => {},
    onEditQueuedMessage: () => {},
    onRemoveQueuedMessage: () => {},
  },
}

// A paused node - resume/cancel live in the ⋮ menu; the popup shows only the
// prompt.
export const PausedResumable: Story = {
  args: {
    node,
    state: { status: 'paused' },
    onClose: () => {},
  },
}

// A needs_input node shows its pending question as a chat bubble; the queue's input widget answers it instead.
export const NeedsInputAnswerable: Story = {
  args: {
    node,
    state: { status: 'needs_input', question: 'Should I include hostel prices, or hotels only?' },
    onClose: () => {},
    onAnswerQuestion: () => {},
  },
}

// If the resume wiring isn't available (e.g. a historical/reloaded turn), the
// question still surfaces, read-only - never blocked on backend work.
export const NeedsInputReadOnly: Story = {
  args: {
    node,
    state: { status: 'needs_input', question: 'Should I include hostel prices, or hotels only?' },
    onClose: () => {},
  },
}
