import { useEffect, useState } from 'react'
import { AssistantText, BubbleHeader } from './AgentParts'
import { type DagNodeDef } from '../state/agentStream'
import type { NodeState, QueuedMessage } from '../state/chatStore'

// NodePopup (#384/#265, restyled for 0.9.0) is an extension of the main chat,
// not a bespoke modal: the node's prompt renders through the same
// BubbleHeader + AssistantText markdown treatment as every chat bubble, with
// no standalone header or section dividers of its own — only a light
// overlay + close affordance to pop it out. Pause/cancel live one click away
// in DagNode's ⋮ menu now; this surface is for what needs the input/editor —
// queueing a message, editing a not-yet-started prompt, or answering a
// pending mid-node question.
interface Props {
  node: DagNodeDef
  state: NodeState
  onClose: () => void
  onQueueMessage?: (nodeId: string, text: string) => void
  onEditQueuedMessage?: (nodeId: string, messageId: string, text: string) => void
  onRemoveQueuedMessage?: (nodeId: string, messageId: string) => void
  onEditTask?: (nodeId: string, task: string) => void
  // Answers a paused node's mid-node question (needs_input) via the same
  // resume path the main chat's QuestionBubble uses (chatStore.submit —
  // the next chat message is delivered to the node as its answer).
  onAnswerQuestion?: (nodeId: string, answer: string) => void
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
          className="flex-1 min-w-0 text-xs px-2 py-1 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-700 dark:text-gray-200 focus:outline-none focus:ring-1 focus:ring-gray-400"
        />
        <button onClick={() => { onEdit?.(text); setEditing(false) }} className="text-[11px] font-medium text-gray-600 dark:text-gray-300 hover:underline">save</button>
        <button onClick={() => { setText(msg.text); setEditing(false) }} className="text-[11px] text-gray-400 dark:text-gray-500 hover:underline">cancel</button>
      </li>
    )
  }
  return (
    <li className="flex items-center gap-2 text-xs text-gray-700 dark:text-gray-200">
      <span className="flex-1 min-w-0">{msg.text}</span>
      {onEdit && <button onClick={() => setEditing(true)} aria-label="Edit" title="Edit" className="text-gray-400 hover:text-gray-600 dark:text-gray-500 dark:hover:text-gray-300">✎</button>}
      {onRemove && <button onClick={onRemove} aria-label="Remove" title="Remove" className="text-gray-400 hover:text-red-500 dark:text-gray-500 dark:hover:text-red-400">✕</button>}
    </li>
  )
}

export function NodePopup({
  node, state, onClose,
  onQueueMessage, onEditQueuedMessage, onRemoveQueuedMessage, onEditTask, onAnswerQuestion,
}: Props) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onClose])

  const [inputText, setInputText] = useState('')
  const [editingTask, setEditingTask] = useState(false)
  const [taskText, setTaskText] = useState(node.task)

  // A node is editable-before-start only while still `queued` (never
  // dispatched) — matches the server's check (PATCH .../nodes/{id}).
  const notStarted = state.status === 'queued'
  const running = state.status === 'running'
  // Answering resumes the node now; queueing waits for its next turn
  // boundary — same input widget, chosen by which state the node is in.
  const answering = state.status === 'needs_input' && state.question != null
  const queue = state.queue ?? []

  function submitInput() {
    const text = inputText.trim()
    if (!text) return
    if (answering) {
      onAnswerQuestion?.(node.id, text)
      setInputText('')
      onClose()
    } else {
      onQueueMessage?.(node.id, text)
      setInputText('')
    }
  }

  return (
    <div
      role="dialog"
      aria-modal="true"
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4"
      onClick={onClose}
    >
      <div
        className="relative w-full max-w-2xl max-h-[85vh] overflow-y-auto rounded-2xl bg-gray-50 dark:bg-gray-900 shadow-xl px-5 pb-6 pt-2 space-y-2"
        onClick={e => e.stopPropagation()}
      >
        {/* Close on its own row so it never overlaps the content bubbles. */}
        <div className="flex justify-end -mb-1">
          <button
            onClick={onClose}
            aria-label="Close"
            className="flex h-7 w-7 items-center justify-center rounded-lg text-gray-400 hover:text-gray-600 hover:bg-gray-200/70 dark:text-gray-500 dark:hover:text-gray-200 dark:hover:bg-gray-700/70 transition-colors"
          >
            ✕
          </button>
        </div>

        {/* Prompt — the same bubble treatment as an assistant turn in chat. */}
        <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-2xl rounded-tl-sm px-5 py-4">
          <div className="flex items-center justify-between">
            <BubbleHeader agent={node.agent} />
            {notStarted && onEditTask && !editingTask && (
              <button onClick={() => { setTaskText(node.task); setEditingTask(true) }} aria-label="Edit prompt" title="Edit prompt" className="shrink-0 text-gray-400 hover:text-gray-600 dark:text-gray-500 dark:hover:text-gray-300">
                ✎
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

        {/* Pending mid-node question — rendered as its own chat-style bubble,
            answered (or read-only, if no resume wiring was passed) below. */}
        {answering && (
          <div className="bg-white dark:bg-gray-800 border border-blue-300 dark:border-blue-700 border-l-4 rounded-2xl rounded-tl-sm px-5 py-4">
            <div className="flex items-center gap-1.5 text-xs font-medium text-blue-700 dark:text-blue-300 mb-1">
              <span aria-hidden="true">❓</span>
              <BubbleHeader agent={node.agent} />
            </div>
            <AssistantText text={state.question ?? ''} />
          </div>
        )}

        {/* Message queue, only while running — plain history, immutable once delivered. */}
        {running && !answering && queue.length > 0 && (
          <div>
            <span className="text-[10px] font-semibold text-gray-400 dark:text-gray-500 uppercase tracking-wide">
              Queued messages
            </span>
            <p className="text-[11px] text-gray-400 dark:text-gray-500 mt-0.5 mb-2">
              Delivered at the node's next turn boundary — never mid-turn.
            </p>
            <ul className="space-y-1.5">
              {queue.map(m => (
                <QueuedMessageRow
                  key={m.id}
                  msg={m}
                  onEdit={onEditQueuedMessage ? (text) => onEditQueuedMessage(node.id, m.id, text) : undefined}
                  onRemove={onRemoveQueuedMessage ? () => onRemoveQueuedMessage(node.id, m.id) : undefined}
                />
              ))}
            </ul>
          </div>
        )}

        {/* One shared input: queues a message on a running node (delivered at
            its next turn boundary), or answers a needs_input node (resumes
            it immediately) — same widget, different destination. */}
        {((running && onQueueMessage) || (answering && onAnswerQuestion)) && (
          <div className="flex items-center gap-2">
            <input
              autoFocus={answering}
              value={inputText}
              onChange={e => setInputText(e.target.value)}
              onKeyDown={e => {
                if (e.key === 'Enter' && inputText.trim()) { e.preventDefault(); submitInput() }
              }}
              placeholder={answering ? 'Type your answer…' : 'Queue a message for this node…'}
              className={`flex-1 min-w-0 text-xs px-2 py-1.5 rounded border bg-white dark:bg-gray-800 text-gray-700 dark:text-gray-200 focus:outline-none focus:ring-1 ${
                answering
                  ? 'border-blue-300 dark:border-blue-700 focus:ring-blue-400'
                  : 'border-gray-300 dark:border-gray-600 focus:ring-gray-400'
              }`}
            />
            <button
              onClick={submitInput}
              disabled={!inputText.trim()}
              aria-label={answering ? 'Send answer' : 'Queue message'}
              title={answering ? 'Send answer' : 'Queue message'}
              className={`px-3 py-1.5 rounded-lg text-white text-xs font-medium disabled:opacity-50 disabled:cursor-not-allowed transition-colors ${
                answering ? 'bg-blue-600 hover:bg-blue-700' : 'bg-gray-700 hover:bg-gray-600 dark:bg-gray-600 dark:hover:bg-gray-500'
              }`}
            >
              ➤
            </button>
          </div>
        )}
        {answering && !onAnswerQuestion && (
          <p className="text-[11px] text-gray-400 dark:text-gray-500 italic">
            Answering from here isn't wired up yet — reply in the main chat.
          </p>
        )}
      </div>
    </div>
  )
}
