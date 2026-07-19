import type { ChatStatus, NodeStatus } from '../generated'

// StatusDot is the app's single "state at a glance" indicator: a bare colored
// dot, shared by the chat list (ChatList) and DAG nodes (DagNode). Quiet states
// (idle, done) render nothing — per the same "the common case stays quiet"
// principle the chat-list dot already followed — so the dot only draws the eye
// when something needs attention or is in flight.
export type DotStatus = ChatStatus | NodeStatus

const COLOR: Partial<Record<DotStatus, string>> = {
  running: 'bg-blue-500',
  needs_input: 'bg-amber-500',
  paused: 'bg-amber-500',
  failed: 'bg-red-500',
  cancelled: 'bg-gray-400 dark:bg-gray-500',
}

const LABEL: Partial<Record<DotStatus, string>> = {
  running: 'Running',
  needs_input: 'Needs input',
  paused: 'Paused',
  failed: 'Failed',
  cancelled: 'Cancelled',
  idle: 'Idle',
  queued: 'Queued',
  done: 'Done',
}

export function StatusDot({ status, className = '', variant = 'node' }: {
  status: DotStatus
  className?: string
  // A chat waiting behind max_active_runs is worth flagging (it's a distinct,
  // in-flight-adjacent state); a DAG node that's merely queued within its own
  // plan is the common case and stays quiet, per the file-level comment above.
  // Node status is the default since DagNode is StatusDot's original caller.
  variant?: 'chat' | 'node'
}) {
  const color = status === 'queued' && variant === 'chat' ? 'bg-gray-400 dark:bg-gray-500' : COLOR[status]
  if (!color) return null
  const label = LABEL[status] ?? status
  // Running pulses so the single dot conveys "live" on its own — the same
  // meaning everywhere it appears (chat list + DAG nodes), replacing the
  // node-only bouncing spinner. Queued never pulses: it isn't actively running.
  const pulse = status === 'running' ? 'animate-pulse' : ''
  return (
    <span
      title={label}
      aria-label={`Status: ${label}`}
      className={`flex-shrink-0 inline-block w-1.5 h-1.5 rounded-full ${color} ${pulse} ${className}`}
    />
  )
}
