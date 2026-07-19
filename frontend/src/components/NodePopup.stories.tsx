import type { Meta, StoryObj } from '@storybook/react-vite'
import { NodePopup } from './NodePopup'
import type { DagNodeDef } from '../state/agentStream'

const meta: Meta<typeof NodePopup> = {
  title: 'Chat/NodePopup',
  component: NodePopup,
  parameters: { layout: 'fullscreen' },
}
export default meta

type Story = StoryObj<typeof NodePopup>

const node: DagNodeDef = {
  id: 'r1',
  agent: 'web-researcher',
  task: '## Task\n\nFind the best time to visit Dublin: climate, peak/off-peak seasons, and rainfall data.\n\n- Cite sources\n- Prefer official meteorological data',
  depends_on: [],
}

// The rendered-markdown prompt, no controls (a completed/historical turn).
export const ReadOnly: Story = {
  args: {
    node,
    state: { status: 'done' },
    onClose: () => {},
  },
}

// A not-yet-started node: the prompt is editable.
export const PendingEditablePrompt: Story = {
  args: {
    node,
    state: { status: 'queued' },
    onClose: () => {},
    onEditTask: () => {},
    onCancel: () => {},
  },
}

// A running node: cancel/pause + the queue input, with one queued message.
export const RunningWithQueue: Story = {
  args: {
    node,
    state: {
      status: 'running',
      queue: [
        { id: 'q1', text: 'Also check winter rainfall.', delivered: false, created_at: new Date().toISOString() },
        { id: 'q0', text: 'Use the last 10 years of data.', delivered: true, created_at: new Date().toISOString() },
      ],
    },
    onClose: () => {},
    onCancel: () => {},
    onPause: () => {},
    onQueueMessage: () => {},
    onEditQueuedMessage: () => {},
    onRemoveQueuedMessage: () => {},
  },
}

// A paused node: resume + cancel.
export const PausedResumable: Story = {
  args: {
    node,
    state: { status: 'paused' },
    onClose: () => {},
    onCancel: () => {},
    onResume: () => {},
  },
}
