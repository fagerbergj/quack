import { useState, useEffect, useCallback } from 'react'
import { DagNode } from './DagNode'
import type { DagTurnState, NodeState } from '../state/chatStore'
import type { MessagePart } from './messageParts'

function fmtMs(ms: number): string {
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(1)}s`
  const m = Math.floor(s / 60)
  const rem = Math.floor(s % 60)
  if (m < 60) return `${m}m ${rem}s`
  const h = Math.floor(m / 60)
  return `${h}h ${m % 60}m ${rem}s`
}

function LiveTimer({ startedAt, finishedAt }: { startedAt: number; finishedAt?: number }) {
  const [now, setNow] = useState(Date.now)
  useEffect(() => {
    if (finishedAt != null) return
    const id = setInterval(() => setNow(Date.now()), 100)
    return () => clearInterval(id)
  }, [finishedAt])
  return <>{fmtMs((finishedAt ?? now) - startedAt)}</>
}

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

// finalNodeId returns the terminal node (no successors).
function finalNodeId(nodeIds: string[], dependsOnMap: Record<string, string[]>): string | null {
  const hasSucessor = new Set<string>()
  for (const id of nodeIds) {
    for (const dep of (dependsOnMap[id] ?? [])) hasSucessor.add(dep)
  }
  return nodeIds.find(id => !hasSucessor.has(id)) ?? null
}

interface Props {
  dag: DagTurnState
}

export function DagView({ dag }: Props) {
  const nodeMap = Object.fromEntries(dag.nodes.map(n => [n.id, n]))
  const dependsOnMap: Record<string, string[]> = {}
  for (const n of dag.nodes) dependsOnMap[n.id] = n.depends_on ?? []

  const nodeIds = dag.nodes.map(n => n.id)
  const layers = topoLayers(nodeIds, dependsOnMap)
  const finalId = finalNodeId(nodeIds, dependsOnMap)

  const getState = (id: string): NodeState =>
    dag.nodeStates[id] ?? { status: 'queued' }
  const getParts = (id: string): MessagePart[] =>
    dag.nodeParts[id] ?? []

  const totalTokens = dag.nodes.reduce((sum, n) => sum + (dag.nodeStates[n.id]?.totalTokens ?? 0), 0)

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
                  parts={getParts(id)}
                  isFinal={id === finalId}
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
