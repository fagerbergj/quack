import { useEffect, useState } from 'react'
import { AssistantText } from './AgentParts'
import { agentLabel } from './messageParts'
import { type DagNodeDef } from '../state/agentStream'
import type { NodeState, QueuedMessage } from '../state/chatStore'

// NodePopup (#384/#265) is the node's detail surface: the full prompt
// rendered as markdown (replacing the old raw inline block in DagNode), plus
// — when the callbacks are present (a live turn) — the node's controls:
// cancel, pause/resume, and the message queue (add/edit/remove a
// not-yet-delivered message), and editing a not-yet-started node's prompt.
interface Props {
  node: DagNodeDef
  state: NodeState
  onClose: () => void
  onCancel?: (nodeId: string) => void
  onPause?: (nodeId: string) => void
  onResume?: (nodeId: string) => void
  onQueueMessage?: (nodeId: string, text: string) => void
  onEditQueuedMessage?: (nodeId: string, messageId: string, text: string) => void
  onRemoveQueuedMessage?: (nodeId: string, messageId: string) => void
  onEditTask?: (nodeId: string, task: string) => void
}

// QueuedMessageRow renders one queue entry: plain text once delivered
// (immutable, history), an inline editor while pending.
function QueuedMessageRow({ msg, onEdit, onRemove }: {
  msg: QueuedMessage
  onEdit?: (text: string) => void
  onRemove?: () => void
}) {
  const [editing, setEditing] = useState(false)
  const [text, setText] = useState(msg.text)
  if (msg.delivered) {
    return (
      <li className="text-xs text-gray-400 dark:text-gray-500 line-through decoration-gray-300 dark:decoration-gray-600">
        {msg.text}
      </li>
    )
  }
  if (editing) {
    return (
      <li className="flex items-center gap-2">
        <input
          autoFocus
          value={text}
          onChange={e => setText(e.target.value)}
          onKeyDown={e => {
            if (e.key === 'Enter') { e.preventDefault(); onEdit?.(text); setEditing(false) }
            if (e.key === 'Escape') { setText(msg.text); setEditing(false) }
          }}
          className="flex-1 min-w-0 text-xs px-2 py-1 rounded border border-amber-300 dark:border-amber-700 bg-white dark:bg-gray-800 text-gray-700 dark:text-gray-200 focus:outline-none focus:ring-1 focus:ring-amber-400"
        />
        <button onClick={() => { onEdit?.(text); setEditing(false) }} className="text-[11px] font-medium text-amber-700 dark:text-amber-400 hover:underline">save</button>
        <button onClick={() => { setText(msg.text); setEditing(false) }} className="text-[11px] text-gray-400 dark:text-gray-500 hover:underline">cancel</button>
      </li>
    )
  }
  return (
    <li className="flex items-center gap-2 text-xs text-gray-700 dark:text-gray-200">
      <span className="flex-1 min-w-0">{msg.text}</span>
      {onEdit && <button onClick={() => setEditing(true)} className="text-[11px] text-amber-600 dark:text-amber-400 hover:underline">edit</button>}
      {onRemove && <button onClick={onRemove} className="text-[11px] text-red-500 dark:text-red-400 hover:underline">remove</button>}
    </li>
  )
}

export function NodePopup({
  node, state, onClose, onCancel, onPause, onResume,
  onQueueMessage, onEditQueuedMessage, onRemoveQueuedMessage, onEditTask,
}: Props) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onClose])

  const [queueText, setQueueText] = useState('')
  const [editingTask, setEditingTask] = useState(false)
  const [taskText, setTaskText] = useState(node.task)

  // A node is editable-before-start only while still `queued` (never
  // dispatched) — matches the server's check (PATCH .../nodes/{id}).
  const notStarted = state.status === 'queued'
  const running = state.status === 'running'
  const paused = state.status === 'paused'
  const cancellable = running || paused || state.status === 'queued' || state.status === 'needs_input'
  const queue = state.queue ?? []

  return (
    <div
      role="dialog"
      aria-modal="true"
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4"
      onClick={onClose}
    >
      <div
        className="w-full max-w-2xl max-h-[85vh] overflow-y-auto rounded-xl border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 shadow-xl"
        onClick={e => e.stopPropagation()}
      >
        <div className="flex items-center gap-2 px-4 py-3 border-b border-gray-100 dark:border-gray-700">
          <span className="text-sm font-semibold text-gray-700 dark:text-gray-200">{agentLabel(node.agent)}</span>
          <span className="text-[10px] text-gray-400 dark:text-gray-500">node {node.id}</span>
          <button onClick={onClose} aria-label="Close" className="ml-auto text-gray-400 dark:text-gray-500 hover:text-gray-600 dark:hover:text-gray-300">
            ✕
          </button>
        </div>

        {/* Prompt: rendered markdown, or an editor when not yet started. */}
        <div className="px-4 py-3 border-b border-gray-100 dark:border-gray-700">
          <div className="flex items-center justify-between mb-1">
            <span className="text-[10px] font-semibold text-gray-400 dark:text-gray-500 uppercase tracking-wide">Prompt</span>
            {notStarted && onEditTask && !editingTask && (
              <button onClick={() => { setTaskText(node.task); setEditingTask(true) }} className="text-[11px] text-indigo-600 dark:text-indigo-400 hover:underline">
                edit
              </button>
            )}
          </div>
          {editingTask ? (
            <div className="space-y-2">
              <textarea
                autoFocus
                value={taskText}
                onChange={e => setTaskText(e.target.value)}
                rows={6}
                className="w-full text-xs px-2 py-1.5 rounded border border-indigo-300 dark:border-indigo-700 bg-white dark:bg-gray-900 text-gray-700 dark:text-gray-200 focus:outline-none focus:ring-1 focus:ring-indigo-400"
              />
              <div className="flex items-center gap-2">
                <button
                  onClick={() => { onEditTask?.(node.id, taskText); setEditingTask(false) }}
                  className="text-[11px] font-medium text-indigo-700 dark:text-indigo-400 hover:underline"
                >
                  save
                </button>
                <button onClick={() => setEditingTask(false)} className="text-[11px] text-gray-400 dark:text-gray-500 hover:underline">cancel</button>
              </div>
            </div>
          ) : (
            <AssistantText text={node.task} />
          )}
        </div>

        {/* Controls: cancel, pause/resume — only on a live turn. */}
        {(onCancel || onPause || onResume) && (
          <div className="flex items-center gap-3 px-4 py-2 border-b border-gray-100 dark:border-gray-700">
            {running && onPause && (
              <button onClick={() => onPause(node.id)} title="Suspend this node, keeping its accumulated work (resumable)"
                className="text-[11px] font-medium text-blue-600 dark:text-blue-400 hover:underline">
                ⏸ pause
              </button>
            )}
            {paused && onResume && (
              <button onClick={() => onResume(node.id)} title="Resume this node with a fresh re-run"
                className="text-[11px] font-medium text-blue-600 dark:text-blue-400 hover:underline">
                ▶ resume
              </button>
            )}
            {cancellable && onCancel && (
              <button onClick={() => onCancel(node.id)} title="Cancel this node immediately (the rest of the run continues)"
                className="text-[11px] font-medium text-red-500 dark:text-red-400 hover:underline">
                ✕ cancel
              </button>
            )}
          </div>
        )}

        {/* Message queue: add/edit/remove, only while running. */}
        {running && onQueueMessage && (
          <div className="px-4 py-3">
            <span className="text-[10px] font-semibold text-gray-400 dark:text-gray-500 uppercase tracking-wide">
              Queued messages
            </span>
            <p className="text-[11px] text-gray-400 dark:text-gray-500 mt-0.5 mb-2">
              Delivered at the node's next turn boundary — never mid-turn.
            </p>
            {queue.length > 0 && (
              <ul className="space-y-1.5 mb-2">
                {queue.map(m => (
                  <QueuedMessageRow
                    key={m.id}
                    msg={m}
                    onEdit={onEditQueuedMessage ? (text) => onEditQueuedMessage(node.id, m.id, text) : undefined}
                    onRemove={onRemoveQueuedMessage ? () => onRemoveQueuedMessage(node.id, m.id) : undefined}
                  />
                ))}
              </ul>
            )}
            <div className="flex items-center gap-2">
              <input
                value={queueText}
                onChange={e => setQueueText(e.target.value)}
                onKeyDown={e => {
                  if (e.key === 'Enter' && queueText.trim()) {
                    e.preventDefault()
                    onQueueMessage(node.id, queueText.trim())
                    setQueueText('')
                  }
                }}
                placeholder="Queue a message for this node…"
                className="flex-1 min-w-0 text-xs px-2 py-1 rounded border border-amber-300 dark:border-amber-700 bg-white dark:bg-gray-800 text-gray-700 dark:text-gray-200 focus:outline-none focus:ring-1 focus:ring-amber-400"
              />
              <button
                onClick={() => { if (queueText.trim()) { onQueueMessage(node.id, queueText.trim()); setQueueText('') } }}
                className="text-[11px] font-medium text-amber-700 dark:text-amber-400 hover:underline"
              >
                queue
              </button>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
