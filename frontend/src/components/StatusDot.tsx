import type { ChatStatus, NodeStatus } from '../generated'

// The chat variant stays quiet for idle/done so a long list flags only what needs attention; the node
// variant always pairs the dot with the state's name so state is never colour-only (WCAG 1.4.1).
export type DotStatus = ChatStatus | NodeStatus

const COLOR: Record<DotStatus, string> = {
  running: 'bg-blue-500',
  needs_input: 'bg-amber-500',
  paused: 'bg-amber-500',
  failed: 'bg-red-500',
  cancelled: 'bg-gray-400 dark:bg-gray-500',
  queued: 'bg-gray-400 dark:bg-gray-500',
  idle: 'bg-gray-300 dark:bg-gray-600',
  done: 'bg-green-500',
}

const LABEL: Record<DotStatus, string> = {
  running: 'Running',
  needs_input: 'Needs input',
  paused: 'Paused',
  failed: 'Failed',
  cancelled: 'Cancelled',
  idle: 'Idle',
  queued: 'Queued',
  done: 'Done',
}

export function StatusDot({ status, className = '', variant = 'node', label }: {
  status: DotStatus
  className?: string
  // Node status is the default since DagNode is StatusDot's original caller.
  variant?: 'chat' | 'node'
  // Node variant only: replaces the generic state name with a more specific
  // one ("needs your answer", "paused · shutdown").
  label?: string
}) {
  if (variant === 'chat' && (status === 'idle' || status === 'done')) return null
  const color = COLOR[status] ?? 'bg-gray-400 dark:bg-gray-500'
  const name = LABEL[status] ?? status
  // Running pulses so the dot conveys "live" on its own, everywhere it appears.
  const pulse = status === 'running' ? 'animate-pulse' : ''
  if (variant === 'chat') {
    return (
      <span
        role="img"
        title={name}
        aria-label={`Status: ${name}`}
        className={`flex-shrink-0 inline-block w-1.5 h-1.5 rounded-full ${color} ${pulse} ${className}`}
      />
    )
  }
  return (
    <span className={`flex-shrink-0 inline-flex items-center gap-1.5 ${className}`}>
      <span aria-hidden="true" className={`inline-block w-1.5 h-1.5 rounded-full ${color} ${pulse}`} />
      <span className="text-[11px] font-medium text-gray-500 dark:text-gray-400">{label ?? name.toLowerCase()}</span>
    </span>
  )
}
