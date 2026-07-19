import { useState, useCallback } from 'react'
import { DagNode } from './DagNode'
import { terminalNodeId, dagTotalTokens, type DagTurnState, type NodeState } from '../state/chatStore'
import type { AgentRun } from './messageParts'
import { LiveTimer } from '../utils/timer'

// topoLayers groups node IDs into layers (layer 0 = no deps, etc.)
function topoLayers(nodeIds: string[], dependsOnMap: Record<string, string[]>): string[][] {
  const inDegree: Record<string, number> = {}
  for (const id of nodeIds) inDegree[id] = 0
  for (const id of nodeIds) {
    for (const dep of (dependsOnMap[id] ?? [])) {
      if (dep in inDegree) inDegree[id]++
    }
  }

  const layers: string[][] = []
  const remaining = new Set(nodeIds)
  while (remaining.size > 0) {
    const layer = [...remaining].filter(id => inDegree[id] === 0)
    if (layer.length === 0) break // cycle guard
    layers.push(layer)
    for (const id of layer) {
      remaining.delete(id)
      for (const other of remaining) {
        if ((dependsOnMap[other] ?? []).includes(id)) {
          inDegree[other]--
        }
      }
    }
  }
  return layers
}

interface Props {
  dag: DagTurnState
  // Present only for a live, streaming run: per-node controls (cancel / pause /
  // resume / queue a message), surfaced in the node popup (#265).
  onCancelNode?: (nodeId: string) => void
  onPauseNode?: (nodeId: string) => void
  onResumeNode?: (nodeId: string) => void
  onQueueNodeMessage?: (nodeId: string, text: string) => void
  onEditQueuedMessage?: (nodeId: string, messageId: string, text: string) => void
  onRemoveQueuedMessage?: (nodeId: string, messageId: string) => void
  onEditNodeTask?: (nodeId: string, task: string) => void
  // Retry a finished node (failed or done) + its downstream. Present when the turn
  // is the live one and not currently streaming.
  onRetryNode?: (nodeId: string, guidance?: string) => void
}

// DagBubbleHeader is the DAG bubble's compact header — its own attribution line,
// separate from the answer bubble's (which credits the terminal node instead).
export function DagBubbleHeader({ dag }: { dag: DagTurnState }) {
  const tokens = dagTotalTokens(dag)
  return (
    <div className="flex items-center gap-2 mb-2 text-[10px] text-gray-400 dark:text-gray-500">
      <span className="font-semibold text-gray-500 dark:text-gray-400">Plan</span>
      <span>· {dag.nodes.length} node{dag.nodes.length === 1 ? '' : 's'}</span>
      {tokens > 0 && <span className="tabular-nums">· {tokens.toLocaleString()} tok</span>}
    </div>
  )
}

export function DagView({
  dag, onCancelNode, onPauseNode, onResumeNode, onQueueNodeMessage,
  onEditQueuedMessage, onRemoveQueuedMessage, onEditNodeTask, onRetryNode,
}: Props) {
  const nodeMap = Object.fromEntries(dag.nodes.map(n => [n.id, n]))
  const dependsOnMap: Record<string, string[]> = {}
  for (const n of dag.nodes) dependsOnMap[n.id] = n.depends_on ?? []

  const nodeIds = dag.nodes.map(n => n.id)
  const layers = topoLayers(nodeIds, dependsOnMap)
  const finalId = terminalNodeId(dag.nodes)

  const getState = (id: string): NodeState =>
    dag.nodeStates[id] ?? { status: 'queued' }
  const getRuns = (id: string): AgentRun[] =>
    dag.nodeRuns[id] ?? []
  const getAnswer = (id: string): string =>
    dag.nodeAnswer[id] ?? ''

  const totalTokens = dagTotalTokens(dag)

  const [copied, setCopied] = useState(false)
  const copyDag = useCallback(() => {
    const payload = {
      nodes: dag.nodes,
      edges: dag.edges,
      nodeStates: dag.nodeStates,
    }
    navigator.clipboard.writeText(JSON.stringify(payload, null, 2)).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }, [dag.nodes, dag.edges, dag.nodeStates])

  return (
    <div className="space-y-4 not-prose">
      {layers.map((layer, li) => (
        <div key={li}>
          <div className={`flex gap-3 items-stretch ${layer.length > 1 ? 'flex-row' : 'flex-col'}`}>
            {layer.map(id => (
              <div key={id} className={layer.length > 1 ? 'flex-1 min-w-0' : ''}>
                <DagNode
                  node={nodeMap[id]}
                  state={getState(id)}
                  runs={getRuns(id)}
                  answer={getAnswer(id)}
                  isFinal={id === finalId}
                  onCancel={onCancelNode}
                  onPause={onPauseNode}
                  onResume={onResumeNode}
                  onQueueMessage={onQueueNodeMessage}
                  onEditQueuedMessage={onEditQueuedMessage}
                  onRemoveQueuedMessage={onRemoveQueuedMessage}
                  onEditTask={onEditNodeTask}
                  onRetry={onRetryNode}
                />
              </div>
            ))}
          </div>
          {li < layers.length - 1 && (
            <div className="flex justify-center my-2">
              <span className="text-gray-300 dark:text-gray-600 text-lg">↓</span>
            </div>
          )}
        </div>
      ))}
      <div className="flex justify-end items-center gap-3">
        <button
          onClick={copyDag}
          className="text-[10px] text-gray-400 dark:text-gray-500 hover:text-gray-600 dark:hover:text-gray-300 transition-colors"
          title="Copy DAG as JSON"
        >
          {copied ? 'copied!' : 'copy json'}
        </button>
        {totalTokens > 0 && (
          <span className="text-[10px] text-gray-400 dark:text-gray-500 tabular-nums">
            {totalTokens.toLocaleString()} tok
          </span>
        )}
        {dag.startedAt != null && (
          <span className="text-[10px] text-gray-400 dark:text-gray-500 tabular-nums">
            total <LiveTimer startedAt={dag.startedAt} finishedAt={dag.finishedAt} />
          </span>
        )}
      </div>
    </div>
  )
}
