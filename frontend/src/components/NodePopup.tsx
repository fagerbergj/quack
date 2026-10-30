import { useState } from 'react'
import { AssistantText, BubbleHeader } from './AgentParts'
import { Icon } from './Icon'
import { Button } from './ui/button'
import { Card } from './ui/card'
import { Dialog, DialogClose, DialogContent } from './ui/dialog'
import { Input, Textarea } from './ui/input'
import { Tooltip } from './ui/tooltip'
import { type DagNodeDef } from '../state/agentStream'
import type { NodeState, QueuedMessage } from '../state/chatStore'

// An extension of the main chat, not a bespoke modal: the prompt renders as a chat bubble under a light overlay.
// Start/stop/pause live in DagNode's ⋮ menu; this surface is for queueing, prompt editing and answering.
interface Props {
  node: DagNodeDef
  state: NodeState
  // Whether the node ever produced a run: tells a never-dispatched 'queued' from a mid-run re-queue.
  // Defaults to false for callers that don't track it.
  dispatched?: boolean
  onClose: () => void
  onQueueMessage?: (nodeId: string, text: string) => void
  onEditQueuedMessage?: (nodeId: string, messageId: string, text: string) => void
  onRemoveQueuedMessage?: (nodeId: string, messageId: string) => void
  onEditTask?: (nodeId: string, task: string) => void
  // Answers a parked node's question the same way QuestionBubble does: chatStore.startNode,
  // with the answer as NodeStartBody.content.
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
      <li className="text-xs text-gray-500 dark:text-gray-400 line-through decoration-gray-300 dark:decoration-gray-600">
        {msg.text}
      </li>
    )
  }
  if (editing) {
    return (
      <li className="flex items-center gap-2">
        <Input
          autoFocus
          value={text}
          onChange={e => setText(e.target.value)}
          onKeyDown={e => {
            if (e.key === 'Enter') { e.preventDefault(); onEdit?.(text); setEditing(false) }
            if (e.key === 'Escape') { setText(msg.text); setEditing(false) }
          }}
          data-editing
          className="flex-1"
        />
        <Button variant="link" size="inline" onClick={() => { onEdit?.(text); setEditing(false) }}>save</Button>
        <Button variant="link" size="inline" className="text-muted-foreground" onClick={() => { setText(msg.text); setEditing(false) }}>cancel</Button>
      </li>
    )
  }
  return (
    <li className="flex items-center gap-2 text-xs text-gray-700 dark:text-gray-200">
      <span className="flex-1 min-w-0">{msg.text}</span>
      <span
        title="Parks until the current round ends - not delivered mid-turn for this node"
        className="shrink-0 text-2xs font-medium text-amber-700 dark:text-amber-400"
      >
        parked
      </span>
      {onEdit && (
        <Tooltip content="Edit">
          <Button variant="ghost" size="icon" onClick={() => setEditing(true)} aria-label="Edit" className="size-6 medium:size-6"><Icon name="edit" className="w-3.5 h-3.5" /></Button>
        </Tooltip>
      )}
      {onRemove && (
        <Tooltip content="Remove">
          <Button variant="ghost" size="icon" onClick={onRemove} aria-label="Remove" className="size-6 medium:size-6 hover:text-destructive"><Icon name="close" className="w-3.5 h-3.5" /></Button>
        </Tooltip>
      )}
    </li>
  )
}

// PromptBlock: the node's prompt as a chat bubble - AssistantText by
// default, an inline editor while a not-yet-started prompt is being edited.
function PromptBlock({ nodeId, agent, task, notStarted, onEditTask }: {
  nodeId: string
  agent: string
  task: string
  notStarted: boolean
  onEditTask?: (nodeId: string, task: string) => void
}) {
  const [editingTask, setEditingTask] = useState(false)
  const [taskText, setTaskText] = useState(task)
  return (
    <>
      <div className="flex items-center justify-between">
        <BubbleHeader agent={agent} />
        {notStarted && onEditTask && !editingTask && (
          <Tooltip content="Edit prompt">
            <Button variant="ghost" size="icon" onClick={() => { setTaskText(task); setEditingTask(true) }} aria-label="Edit prompt" className="size-6 medium:size-6">
              <Icon name="edit" className="w-3.5 h-3.5" />
            </Button>
          </Tooltip>
        )}
      </div>
      {editingTask ? (
        <div className="space-y-2">
          <Textarea
            autoFocus
            value={taskText}
            onChange={e => setTaskText(e.target.value)}
            rows={6}
          />
          <div className="flex items-center gap-2">
            <Button variant="link" size="inline" onClick={() => { onEditTask?.(nodeId, taskText); setEditingTask(false) }}>save</Button>
            <Button variant="link" size="inline" className="text-muted-foreground" onClick={() => setEditingTask(false)}>cancel</Button>
          </div>
        </div>
      ) : (
        <AssistantText text={task} />
      )}
    </>
  )
}

// QueueSection: the running node's queued messages - plain history,
// immutable once delivered.
function QueueSection({ nodeId, queue, onEditQueuedMessage, onRemoveQueuedMessage }: {
  nodeId: string
  queue: QueuedMessage[]
  onEditQueuedMessage?: (nodeId: string, messageId: string, text: string) => void
  onRemoveQueuedMessage?: (nodeId: string, messageId: string) => void
}) {
  if (queue.length === 0) return null
  return (
    <div>
      <span className="text-2xs font-semibold text-gray-500 dark:text-gray-400">
        Queued messages
      </span>
      <p className="text-2xs text-gray-500 dark:text-gray-400 mt-0.5 mb-2">
        Delivered into the live round when possible; "parked" ones wait for the node's next turn boundary.
      </p>
      <ul className="space-y-2">
        {queue.map(m => (
          <QueuedMessageRow
            key={m.id}
            msg={m}
            onEdit={onEditQueuedMessage ? (text) => onEditQueuedMessage(nodeId, m.id, text) : undefined}
            onRemove={onRemoveQueuedMessage ? () => onRemoveQueuedMessage(nodeId, m.id) : undefined}
          />
        ))}
      </ul>
    </div>
  )
}

// InputRow: the shared input that queues a message on a running node or
// answers a needs_input node - same widget, different destination.
function InputRow({ answering, value, onChange, onSubmit }: {
  answering: boolean
  value: string
  onChange: (text: string) => void
  onSubmit: () => void
}) {
  return (
    <div className="flex items-center gap-2">
      <Input
        autoFocus={answering}
        data-autofocus={answering || undefined}
        value={value}
        onChange={e => onChange(e.target.value)}
        onKeyDown={e => {
          if (e.key === 'Enter' && value.trim()) { e.preventDefault(); onSubmit() }
        }}
        placeholder={answering ? 'Type your answer…' : 'Queue a message for this node…'}
        className={answering ? 'flex-1 border-primary' : 'flex-1'}
      />
      <Tooltip content={answering ? 'Send answer' : 'Queue message'}>
        <Button
          variant={answering ? 'default' : 'secondary'}
          size="icon"
          onClick={onSubmit}
          disabled={!value.trim()}
          aria-label={answering ? 'Send answer' : 'Queue message'}
        >
          <Icon name="send" className="w-4 h-4" />
        </Button>
      </Tooltip>
    </div>
  )
}

// 'queued' also fires mid-run at a worker/judge admission re-wait; dispatched (the node has
// produced a run) tells that apart from the node's real first wait.
export function liveState(status: NodeState['status'], dispatched: boolean) {
  return {
    notStarted: status === 'queued' && !dispatched,
    running: status === 'running' || (status === 'queued' && dispatched),
  }
}

export function NodePopup({
  node, state, dispatched = false, onClose,
  onQueueMessage, onEditQueuedMessage, onRemoveQueuedMessage, onEditTask, onAnswerQuestion,
}: Props) {
  const [inputText, setInputText] = useState('')

  // A node is editable-before-start only while still `queued` AND never
  // dispatched - matches the server's check (PATCH .../nodes/{id}).
  const { notStarted: nodeNotStarted, running } = liveState(state.status, dispatched)
  // Answering resumes the node now; queueing waits for its next turn boundary. needs_input (legacy DB/SSE) and
  // paused/awaiting_input (REST read model) both mean "parked on a question".
  const answering = (state.status === 'needs_input' || state.pauseReason === 'awaiting_input') && state.question != null
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
    <Dialog open onOpenChange={o => { if (!o) onClose() }}>
     <DialogContent
      title={`Node ${node.agent}`}
      className="gap-2 overflow-y-auto px-5 pt-2 medium:pb-6"
      // Escape inside the queued-message editor cancels the edit (its own handler), not the whole popup.
      onEscapeKeyDown={e => { if (e.target instanceof HTMLElement && e.target.hasAttribute('data-editing')) e.preventDefault() }}
     >
      {/* Close on its own row so it never overlaps the content bubbles. */}
      <div className="flex justify-end -mb-2">
        <DialogClose asChild>
          <Button variant="ghost" size="icon" aria-label="Close" className="-me-3">
            <Icon name="close" className="w-5 h-5" />
          </Button>
        </DialogClose>
      </div>

      {/* Prompt - the same bubble treatment as an assistant turn in chat. */}
      <Card className="rounded-2xl rounded-tl-sm px-5 py-4">
        <PromptBlock nodeId={node.id} agent={node.agent} task={node.task} notStarted={nodeNotStarted} onEditTask={onEditTask} />
      </Card>

      {/* Pending mid-node question - rendered as its own chat-style bubble,
          answered (or read-only, if no resume wiring was passed) below. */}
      {answering && (
        <Card className="border-l-4 border-primary rounded-2xl rounded-tl-sm px-5 py-4">
          <div className="flex items-center gap-2 text-xs font-medium text-blue-700 dark:text-blue-300 mb-1">
            <Icon name="help" className="w-3.5 h-3.5" />
            <BubbleHeader agent={node.agent} />
          </div>
          <AssistantText text={state.question ?? ''} />
        </Card>
      )}

      {/* Message queue, only while running - plain history, immutable once delivered. */}
      {running && !answering && (
        <QueueSection nodeId={node.id} queue={queue} onEditQueuedMessage={onEditQueuedMessage} onRemoveQueuedMessage={onRemoveQueuedMessage} />
      )}

      {/* One shared input: queues on a running node (delivered at its next turn boundary)
          or answers a needs_input node (resumes it immediately). */}
      {((running && onQueueMessage) || (answering && onAnswerQuestion)) && (
        <InputRow answering={answering} value={inputText} onChange={setInputText} onSubmit={submitInput} />
      )}
      {answering && !onAnswerQuestion && (
        <p className="text-2xs text-gray-500 dark:text-gray-400 italic">
          Answering from here isn't wired up yet - reply in the main chat.
        </p>
      )}
     </DialogContent>
    </Dialog>
  )
}
