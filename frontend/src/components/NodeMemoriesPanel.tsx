import { useCallback, useEffect, useState } from 'react'
import { api, type NodeMemory, type VoteDirection } from '../api'
import { Icon, type IconName } from './Icon'
import { VoteControl } from './VoteControl'

export interface NodeMemoriesPanelProps {
  chatId: string
  nodeId: string
  // Bumping this (the node's judge-round counter, chatStore's
  // NodeState.judgeRounds) triggers a refetch - the chat's SSE stream
  // already carries judge-round completion, so this is the "live update"
  // hook the epic #1255 P4 spec asks for without a dedicated subscription.
  judgeRounds?: number
  onClose: () => void
}

const VOTE_ICON: Record<string, IconName> = {
  supported: 'check_circle',
  contradicted: 'cancel',
  not_relevant: 'do_not_disturb',
}

const VOTE_COLOR: Record<string, string> = {
  supported: 'text-green-600 dark:text-green-400',
  contradicted: 'text-red-600 dark:text-red-400',
  not_relevant: 'text-gray-400 dark:text-gray-500',
}

function JudgeVoteIcon({ vote, reason }: { vote: string; reason?: string }) {
  const icon = VOTE_ICON[vote]
  if (!icon) return null
  return (
    <span
      title={reason ? `${vote}: ${reason}` : vote}
      className={`inline-flex items-center gap-1 text-[11px] ${VOTE_COLOR[vote]}`}
    >
      <Icon name={icon} className="w-3.5 h-3.5" label={vote} />
      {vote.replace('_', ' ')}
    </span>
  )
}

// SourceBadge: prefill (injected before the worker started) vs tool (a
// recall_memory call mid-round, epic #1255 P2) - the two ways a memory can
// reach a worker.
function SourceBadge({ source }: { source: string }) {
  return (
    <span className="inline-flex items-center px-1.5 py-0.5 rounded-full text-[10px] font-medium bg-gray-100 text-gray-500 dark:bg-gray-700 dark:text-gray-400">
      {source}
    </span>
  )
}

// One row: the memory's content, source, tier, the judge's vote (if the
// round has voted yet), and a manual vote control. NodeMemory carries no
// corpus-wide vote_score (that lives on the full Memory the memory page
// shows) - this control only reflects the caller's own vote highlight.
function NodeMemoryRow({ memory, onVote }: { memory: NodeMemory; onVote: (id: string, vote: VoteDirection) => Promise<void> }) {
  return (
    <div className="px-3 py-2.5 border-b border-gray-100 dark:border-gray-700 flex items-start gap-2">
      <VoteControl score={0} ownVote={memory.own_vote} onVote={v => onVote(memory.id, v)} />
      <div className="flex-1 min-w-0">
        {memory.content && <p className="text-sm text-gray-800 dark:text-gray-100 whitespace-pre-wrap">{memory.content}</p>}
        <div className="flex flex-wrap items-center gap-1.5 mt-1.5">
          <SourceBadge source={memory.source} />
          {memory.tier && (
            <span className="inline-flex items-center px-1.5 py-0.5 rounded-full text-[10px] font-medium bg-blue-100 text-blue-700 dark:bg-blue-900/40 dark:text-blue-400">
              {memory.tier}
            </span>
          )}
          {memory.score != null && (
            <span className="text-[11px] text-gray-400 dark:text-gray-500">score {memory.score.toFixed(2)}</span>
          )}
          {memory.vote && <JudgeVoteIcon vote={memory.vote} reason={memory.reason} />}
        </div>
      </div>
    </div>
  )
}

// NodeMemoriesPanel (epic #1255 P4): the memories a worker node received
// (prefill and/or recall_memory tool calls), with the judge's per-memory
// vote once the round has judged them, and a manual vote control. Read from
// GET /api/v1/chats/{id}/nodes/{node}/memories.
export function NodeMemoriesPanel({ chatId, nodeId, judgeRounds, onClose }: NodeMemoriesPanelProps) {
  const [memories, setMemories] = useState<NodeMemory[]>([])
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  const load = useCallback(() => {
    setLoading(true)
    return api.listNodeMemories(chatId, nodeId)
      .then(r => { setMemories(r.memories); setError(null) })
      .catch(e => setError(e instanceof Error ? e.message : 'Failed to load memories'))
      .finally(() => setLoading(false))
  }, [chatId, nodeId])

  useEffect(() => { void load() }, [load, judgeRounds])

  async function handleVote(id: string, vote: VoteDirection) {
    const prev = memories
    setMemories(cur => cur.map(m => (m.id === id ? { ...m, own_vote: vote === 'none' ? undefined : vote } : m)))
    try {
      await api.voteMemory(id, vote)
    } catch (e) {
      setMemories(prev)
      throw e
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onClick={onClose}>
      <div
        role="dialog"
        aria-label="Memories received by this node"
        className="w-full max-w-lg max-h-[80vh] flex flex-col rounded-lg bg-white dark:bg-gray-800 shadow-xl"
        onClick={e => e.stopPropagation()}
      >
        <div className="flex items-center justify-between px-4 py-3 border-b border-gray-200 dark:border-gray-700">
          <h2 className="text-sm font-semibold text-gray-800 dark:text-gray-100">Memories</h2>
          <button
            onClick={onClose}
            aria-label="Close"
            className="min-w-[44px] min-h-[44px] flex items-center justify-center rounded text-gray-400 hover:text-gray-600 dark:hover:text-gray-300"
          >
            <Icon name="close" className="w-4 h-4" />
          </button>
        </div>
        <div className="flex-1 overflow-y-auto">
          {loading && <div className="text-center text-gray-400 dark:text-gray-500 text-sm py-10">Loading…</div>}
          {!loading && error && (
            <div className="m-3 rounded-md bg-red-50 dark:bg-red-950/30 border border-red-200 dark:border-red-800 px-4 py-3 text-sm text-red-700 dark:text-red-400">
              {error}
            </div>
          )}
          {!loading && !error && memories.length === 0 && (
            <div className="text-center text-gray-400 dark:text-gray-500 text-sm py-10">This node received no memories</div>
          )}
          {!loading && !error && memories.map(m => <NodeMemoryRow key={m.id} memory={m} onVote={handleVote} />)}
        </div>
      </div>
    </div>
  )
}
