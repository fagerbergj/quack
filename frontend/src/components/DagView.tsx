import { DagNode } from './DagNode'
import type { DagTurnState, NodeState } from '../state/chatStore'
import type { MessagePart } from './messageParts'

function fmtMs(ms: number): string {
  if (ms < 1000) return `${ms}ms`
  return `${(ms / 1000).toFixed(1)}s`
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

  const overallMs = dag.startedAt != null
    ? (dag.finishedAt ?? Date.now()) - dag.startedAt
    : null

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
      {overallMs != null && (
        <div className="flex justify-end">
          <span className="text-[10px] text-gray-400 dark:text-gray-500 tabular-nums">
            total {fmtMs(overallMs)}
          </span>
        </div>
      )}
    </div>
  )
}
