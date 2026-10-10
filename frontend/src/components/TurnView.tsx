import { memo, useMemo } from 'react'
import { AssistantText, ActivityList, BubbleHeader, stoppedBadge, type StoppedBadge } from './AgentParts'
import { QuestionBubble } from './QuestionBubble'
import { DagView, DagBubbleHeader } from './DagView'
import { TriggerMessage } from './TriggerEnvelope'
import { AttachmentPreviews, type AttachmentPreview } from './AttachmentUI'
import { TurnSurfaces } from './A2uiArtifact'
import type { SurfaceRef } from '../lib/a2ui'
import { dagFromTurn, textFromTurn, stoppedFromTurn, activityFromTurn, dagAnswerAttribution, plainReplyAttribution, dagTurnStateFromItem, type DagTurnState } from '../state/chatStore'
import { pendingChoice, type Activity } from './messageParts'
import type { Turn } from '../generated'

// get_user_choice calls render as a QuestionBubble, so the raw tool block would be redundant.
// Shared by TurnView (completed turns) and Chat (live turn).
export function visibleActivity(activity: Activity[]): Activity[] {
  return activity.filter(a => !(a.kind === 'tool' && a.tool.name === 'get_user_choice'))
}

export interface TurnViewProps {
  turn: Turn
  idx: number
  // Present only for a real chat - gates each node's Artifacts button.
  chatId?: string
  // The answer to this turn's clarification (next turn's input / the live input);
  // undefined means the clarification is still answerable.
  choiceAnswer?: string
  // This turn's input is itself the answer to the previous turn's clarification.
  isChoiceAnswer: boolean
  submittingChoice: boolean
  isCopied: boolean
  // Earlier turns' raw envelope text, oldest first, so a GitHub trigger's <comments> section can show
  // the running history instead of only this trigger's slice.
  priorContents: string[]
  // Resolved from the chat's turn_id-tagged artifact revisions; empty means no thumbnail, never an error.
  imageAttachments?: AttachmentPreview[]
  // a2ui_surface artifacts this turn created, rendered at their latest revision.
  surfaces?: SurfaceRef[]
  onChoice: (option: string) => void
  onCopy: (key: string, text: string) => void
  onDownload: (text: string, idx: number) => void
}

// TurnDagBubble is the completed turn's DAG card - the collapsed "Steps"
// details (activity + graph), as opposed to the live turn's running view.
function TurnDagBubble({ dag, activity, chatId }: { dag: DagTurnState; activity: Activity[]; chatId?: string }) {
  return (
    <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-2xl rounded-tl-sm px-5 py-4">
      <DagBubbleHeader dag={dag} />
      <details className="rounded-lg border border-gray-200 dark:border-gray-700">
        <summary className="cursor-pointer select-none px-3 py-2 text-xs text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300">
          Steps
        </summary>
        <div className="p-2 space-y-3">
          {activity.length > 0 && <ActivityList activity={activity} />}
          <DagView dag={dag} chatId={chatId} />
        </div>
      </details>
    </div>
  )
}

// TurnAnswerBubble is the completed turn's answer card: the attributed
// header, the (DAG-less) activity, and the markdown answer.
function TurnAnswerBubble({ dagState, activity, text, attribution, stopped }: { dagState: DagTurnState | undefined; activity: Activity[]; text: string | undefined; attribution: ReturnType<typeof dagAnswerAttribution> | undefined; stopped: StoppedBadge | undefined }) {
  return (
    <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-2xl rounded-tl-sm px-5 py-4">
      <BubbleHeader agent={attribution?.agent ?? 'orchestrator'} model={attribution?.model} tokens={attribution?.tokens} stopped={stopped} />
      {!dagState && activity.length > 0 && <ActivityList activity={activity} />}
      {text && <AssistantText text={text} />}
    </div>
  )
}

// CopyDownloadRow is the copy/download action pair under an answer that has
// text - the live turn renders the same markup inline in Chat.tsx.
function CopyDownloadRow({ text, copyKey, isCopied, onCopy, onDownload, idx }: { text: string; copyKey: string; isCopied: boolean; onCopy: (key: string, text: string) => void; onDownload: (text: string, idx: number) => void; idx: number }) {
  return (
    <div className="flex items-center gap-3 mt-1.5 px-1">
      <button
        onClick={() => onCopy(copyKey, text)}
        className="min-h-[44px] -my-2 inline-flex items-center text-xs text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 transition-colors"
      >
        {isCopied ? 'Copied!' : 'Copy'}
      </button>
      <button
        onClick={() => onDownload(text, idx)}
        className="min-h-[44px] -my-2 inline-flex items-center text-xs text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 transition-colors"
      >
        Download
      </button>
    </div>
  )
}

// Memoized: completed turns are immutable, so this skips re-parsing markdown/DAG on every streaming
// token of a later turn.
export const TurnView = memo(function TurnView({
  turn, idx, chatId, choiceAnswer, isChoiceAnswer, submittingChoice, isCopied, priorContents, imageAttachments, surfaces, onChoice, onCopy, onDownload,
}: TurnViewProps) {
  const dagItem = dagFromTurn(turn)
  const dagState = dagItem ? dagTurnStateFromItem(dagItem) : undefined
  const text = textFromTurn(turn)
  const turnRuns = activityFromTurn(turn)
  const turnActivity = visibleActivity(turnRuns.flatMap(r => r.activity))
  const turnChoice = pendingChoice(turnRuns)
  // A DAG turn credits its terminal node; a plain reply credits the orchestrator with turn.model and
  // Turn.usage, so history attribution matches the live stream.
  const attribution = dagState ? dagAnswerAttribution(dagState, text) : plainReplyAttribution(turn)
  // Skip the answer bubble when the turn produced no visible content for it
  // (e.g. a DAG with no text yet, or a plain turn that only held a tool call).
  const stopped = stoppedBadge(stoppedFromTurn(turn), text)
  const hasAnswerContent = dagState ? (!!text || !!stopped) : (turnActivity.length > 0 || !!text)
  const copyKey = `turn-${turn.id}`
  // Stable element identity so memo(TriggerMessage) bails on the persisted path too.
  const attachmentsEl = useMemo(
    () => (imageAttachments?.length ? <AttachmentPreviews previews={imageAttachments} /> : undefined),
    [imageAttachments],
  )
  return (
    <div>
      {/* Hidden for a clarification answer, or a label/webhook-triggered turn with no typed message
          (its synthesized task renders in the DAG bubble). */}
      {!isChoiceAnswer && turn.input.content && (
        <TriggerMessage
          content={turn.input.content}
          priorContents={priorContents}
          attachments={attachmentsEl}
          chatId={chatId}
        />
      )}
      {/* Assistant response: DAG bubble → answer bubble, as siblings */}
      <div className="flex justify-start">
        <div className="w-full space-y-3">
          {dagState && <TurnDagBubble dag={dagState} activity={turnActivity} chatId={chatId} />}
          {hasAnswerContent && <TurnAnswerBubble dagState={dagState} activity={turnActivity} text={text} attribution={attribution} stopped={stopped} />}
          {turnChoice && (
            <QuestionBubble
              agent="orchestrator"
              question={turnChoice.question}
              options={turnChoice.options}
              disabled={submittingChoice}
              answered={choiceAnswer}
              onSelect={onChoice}
            />
          )}
          {text && <CopyDownloadRow text={text} copyKey={copyKey} isCopied={isCopied} onCopy={onCopy} onDownload={onDownload} idx={idx} />}
        </div>
      </div>
      <TurnSurfaces chatId={chatId} surfaces={surfaces} />
    </div>
  )
})
