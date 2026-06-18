import { memo } from 'react'
import { AssistantText, ActivityList } from './AgentParts'
import { ChoicePrompt } from './ChoicePrompt'
import { DagView } from './DagView'
import { dagFromTurn, textFromTurn, activityFromTurn, type DagTurnState } from '../state/chatStore'
import { pendingChoice, type Activity } from './messageParts'
import type { Turn, DagOutputItem } from '../generated'

// visibleActivity hides get_user_choice tool calls from the activity log — they are
// surfaced separately as a ChoicePrompt button group, so showing the raw tool block
// too would be redundant. Shared by TurnView (completed turns) and Chat (live turn).
export function visibleActivity(activity: Activity[]): Activity[] {
  return activity.filter(a => !(a.kind === 'tool' && a.tool.name === 'get_user_choice'))
}

// dagTurnStateFromItem converts a persisted DagOutputItem into a DagTurnState
// suitable for DagView. Runs/answers are empty (streaming content isn't persisted).
export function dagTurnStateFromItem(item: DagOutputItem): DagTurnState {
  const nodeStates: DagTurnState['nodeStates'] = {}
  let startedAt: number | undefined
  let finishedAt: number | undefined
  for (const [id, ns] of Object.entries(item.node_states)) {
    nodeStates[id] = {
      status: ns.status as DagTurnState['nodeStates'][string]['status'],
      outputPreview: ns.output_preview,
      error: ns.error,
      startedAt: ns.started_at_ms,
      finishedAt: ns.finished_at_ms,
      model: ns.model,
      promptTokens: ns.prompt_tokens,
      completionTokens: ns.completion_tokens,
      totalTokens: ns.total_tokens,
      finishReason: ns.finish_reason,
      serverDurationMs: ns.server_duration_ms,
    }
    if (ns.started_at_ms != null)
      startedAt = startedAt == null ? ns.started_at_ms : Math.min(startedAt, ns.started_at_ms)
    if (ns.finished_at_ms != null)
      finishedAt = finishedAt == null ? ns.finished_at_ms : Math.max(finishedAt, ns.finished_at_ms)
  }
  return {
    planId: item.plan_id,
    nodes: item.nodes,
    edges: item.edges,
    nodeStates,
    nodeRuns: {},
    nodeAnswer: {},
    startedAt,
    finishedAt,
  }
}

export interface TurnViewProps {
  turn: Turn
  idx: number
  // The answer to this turn's clarification (next turn's input / the live input);
  // undefined means the clarification is still answerable.
  choiceAnswer?: string
  // This turn's input is itself the answer to the previous turn's clarification.
  isChoiceAnswer: boolean
  submittingChoice: boolean
  isCopied: boolean
  onChoice: (option: string) => void
  onCopy: (key: string, text: string) => void
  onDownload: (text: string, idx: number) => void
}

// TurnView renders one completed turn. Memoized: completed turns are immutable, so
// this stops re-rendering (and re-parsing markdown/DAG) on every streaming token of
// a later turn — the props only change for the one turn being copied/answered.
export const TurnView = memo(function TurnView({
  turn, idx, choiceAnswer, isChoiceAnswer, submittingChoice, isCopied, onChoice, onCopy, onDownload,
}: TurnViewProps) {
  const dagItem = dagFromTurn(turn)
  const dagState = dagItem ? dagTurnStateFromItem(dagItem) : undefined
  const text = textFromTurn(turn)
  const turnRuns = activityFromTurn(turn)
  const turnActivity = visibleActivity(turnRuns.flatMap(r => r.activity))
  const turnChoice = pendingChoice(turnRuns)
  // Skip the assistant bubble when the turn produced no visible content
  // (e.g. it only held the get_user_choice call) — avoids a blank box.
  const hasBubbleContent = !!dagState || turnActivity.length > 0 || !!text
  const copyKey = `turn-${turn.id}`
  return (
    <div>
      {/* User message (hidden when it's a clarification answer) */}
      {!isChoiceAnswer && (
        <div className="flex justify-end mb-3">
          <div className="max-w-2xl ml-auto">
            <div className="bg-blue-600 text-white rounded-2xl rounded-tr-sm px-4 py-3 text-sm whitespace-pre-wrap">
              {turn.input.content}
            </div>
          </div>
        </div>
      )}
      {/* Assistant response */}
      <div className="flex justify-start">
        <div className="w-full">
          {hasBubbleContent && (
          <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-2xl rounded-tl-sm px-5 py-4">
            {dagState ? (
              <>
                <details className="mb-4 rounded-lg border border-gray-200 dark:border-gray-700">
                  <summary className="cursor-pointer select-none px-3 py-2 text-xs text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300">
                    ▸ Research steps
                  </summary>
                  <div className="p-2 space-y-3">
                    {turnActivity.length > 0 && <ActivityList activity={turnActivity} />}
                    <DagView dag={dagState} />
                  </div>
                </details>
                {text && <AssistantText text={text} />}
              </>
            ) : (
              <>
                {turnActivity.length > 0 && <ActivityList activity={turnActivity} />}
                {text && <AssistantText text={text} />}
              </>
            )}
          </div>
          )}
          {turnChoice && (
            <ChoicePrompt
              question={turnChoice.question}
              options={turnChoice.options}
              disabled={submittingChoice}
              answered={choiceAnswer}
              onSelect={onChoice}
            />
          )}
          {text && (
            <div className="flex items-center gap-3 mt-1.5 px-1">
              <button
                onClick={() => onCopy(copyKey, text)}
                className="text-xs text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 transition-colors"
              >
                {isCopied ? 'Copied!' : 'Copy'}
              </button>
              <button
                onClick={() => onDownload(text, idx)}
                className="text-xs text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 transition-colors"
              >
                Download
              </button>
            </div>
          )}
        </div>
      </div>
    </div>
  )
})
