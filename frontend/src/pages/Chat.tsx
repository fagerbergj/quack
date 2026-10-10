import { useState, useEffect, useLayoutEffect, useRef, useCallback, useMemo } from 'react'
import { navigate, useChatId } from '../router'
import { api, type ChatSummary } from '../api'
import { AssistantText, ActivityList, LiveStatusLine, BubbleHeader, Dots, stoppedBadge } from '../components/AgentParts'
import { QuestionBubble } from '../components/QuestionBubble'
import { DagView, DagBubbleHeader } from '../components/DagView'
import { Composer } from '../components/Composer'
import { ChatList } from '../components/ChatList'
import { TurnView, visibleActivity } from '../components/TurnView'
import { useChatStore, useChatState } from '../state/ChatStoreProvider'
import { activityFromTurn, dagFromTurn, liveAnswerText, pendingNodeQuestion, dagAnswerAttribution, sessionModels, type DagTurnState, type ChatState } from '../state/chatStore'
import { UsageSummary, type UsageSummaryProps } from '../components/UsageSummary'
import { pendingChoice, showLiveSpinner } from '../components/messageParts'
import { AttachmentPreviews } from '../components/AttachmentUI'
import { GitHubLink } from '../components/GitHubLink'
import { TriggerMessage } from '../components/TriggerEnvelope'
import { ChatMenu } from '../components/ChatMenu'
import { NavToggle } from '../components/NavToggle'
import { Icon } from '../components/Icon'
import { StatusDot } from '../components/StatusDot'
import { LiveTimer } from '../utils/timer'
import type { ChatStatus, Turn } from '../generated'
import { useTurnArtifacts } from '../hooks/useTurnArtifacts'
import type { SurfaceRef } from '../lib/a2ui'
import { TurnSurfaces } from '../components/A2uiArtifact'
import { TONE } from '../lib/colorHash'

// Never shortens the list: the poll must not truncate a sidebar paged past the page cap. Safe because `page` is
// exactly the current top N by updated_at, so nothing left in `existing` can outrank it.
export function mergeChatsPage(existing: ChatSummary[], page: ChatSummary[]): ChatSummary[] {
  const pageIds = new Set(page.map(c => c.id))
  return [...page, ...existing.filter(c => !pageIds.has(c.id))]
}

// A poll in flight during an archive PATCH can still list the chat as active; mergeChatsPage trusts `page`,
// so that stale entry would undo the archive.
export function pollPageExcludingPending(page: ChatSummary[], pendingIds: Set<string>): ChatSummary[] {
  return pendingIds.size === 0 ? page : page.filter(c => !pendingIds.has(c.id))
}

// Archiving into an unloaded section must not seed it: undefined is how handleExpandArchived knows the first
// page still needs fetching.
export function nextArchivedChats(current: ChatSummary[] | undefined, item: ChatSummary, archived: boolean): ChatSummary[] | undefined {
  if (archived) return current === undefined ? undefined : [item, ...current]
  return current?.filter(c => c.id !== item.id)
}

// Falls back to the GetChat snapshot for an archived chat, which must stay out of the active-scoped `chats`.
export function resolveActiveChat(chats: ChatSummary[], activeChatId: string | null, detail: ChatSummary | null): ChatSummary | undefined {
  return chats.find(s => s.id === activeChatId) ?? (detail?.id === activeChatId ? detail : undefined)
}

// A backgrounded tab has nothing to show, so it skips polls; becoming visible polls at once.
export function pollWhileVisible(poll: () => void, intervalMs: number): () => void {
  function tick() {
    if (!document.hidden) poll()
  }
  document.addEventListener('visibilitychange', tick)
  const id = setInterval(tick, intervalMs)
  return () => {
    clearInterval(id)
    document.removeEventListener('visibilitychange', tick)
  }
}

// github_url is set only by the webhook at dispatch, so a direct chat gets null.
export function chatGitHubLink(chat: ChatSummary | undefined): { url: string; repo?: string } | null {
  if (!chat?.github_url) return null
  return { url: chat.github_url, repo: chat.github_repo }
}

export interface EditableChatTitleProps {
  title: string
  editable: boolean
  onRename: (title: string) => void
}

// Enter/blur commits, Escape cancels; a blank or unchanged draft is a no-op, never a rename to ''.
export function EditableChatTitle({ title, editable, onRename }: EditableChatTitleProps) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState('')
  const inputRef = useRef<HTMLInputElement>(null)

  function startEdit() {
    if (!editable) return
    setDraft(title)
    setEditing(true)
  }

  function commit() {
    setEditing(false)
    const next = draft.trim()
    if (next && next !== title) onRename(next)
  }

  if (editing) {
    return (
      <input
        ref={inputRef}
        autoFocus
        value={draft}
        onChange={e => setDraft(e.target.value)}
        onBlur={commit}
        onKeyDown={e => {
          if (e.key === 'Enter') inputRef.current?.blur()
          if (e.key === 'Escape') setEditing(false)
        }}
        aria-label="Chat title"
        className="text-base font-semibold text-gray-900 dark:text-white bg-transparent border-b border-blue-500 focus:outline-none min-w-0 flex-1"
      />
    )
  }

  return (
    <h1
      onClick={startEdit}
      title={editable ? 'Click to rename' : undefined}
      className={`group flex items-center gap-1.5 text-base font-semibold text-gray-900 dark:text-white ${editable ? 'cursor-text' : ''}`}
    >
      {/* One line at every width: the compact header's second line is the run status; title holds the full text. */}
      <span className="truncate" title={title}>{title}</span>
      {editable && (
        <Icon name="edit" className="opacity-0 group-hover:opacity-100 text-gray-400 w-3.5 h-3.5 transition-opacity flex-shrink-0" />
      )}
    </h1>
  )
}

// Recent completed turns mounted by default; "Show N older messages" raises it by the same amount.
const RENDERED_TURN_TAIL = 100

// Keeps a live DAG visible without scrolling to the node cards.
export function ChatHeaderStatus({ status, startedAt }: { status: ChatStatus; startedAt?: number }) {
  return (
    <span className="flex-shrink-0 inline-flex items-center gap-1.5 text-xs text-gray-500 dark:text-gray-400">
      <StatusDot status={status} variant="chat" />
      {startedAt != null && <span className="tabular-nums"><LiveTimer startedAt={startedAt} /></span>}
    </span>
  )
}

// App.tsx owns the drawer state so the header toggle and the NavRail overlay share one source of truth.
export interface ChatProps {
  navOpen: boolean
  onToggleNav: () => void
}

// Keeps LiveTurnView a plain render.
function liveTurnDerived(live: NonNullable<ChatState['live']>, liveActive: boolean) {
  const liveDag = live.dag
  const liveTopText = live.text ?? ''
  const liveTopRuns = live.runs ?? []
  const liveDone = !liveActive
  // With a DAG the terminal node's answer is the reply; liveTopText is then only orchestrator narration, and
  // falling back to it would mask a missing terminal answer.
  const liveText = liveDag ? liveAnswerText(live) : liveTopText
  // get_user_choice is surfaced as its own QuestionBubble, not a raw tool block.
  const orchActivity = visibleActivity(liveTopRuns.flatMap(r => r.activity))
  // Keyed on visible activity, not run count: the top-level run is created empty on the first event, so a
  // run-count check blanks the dots before the plan.
  const showSpinner = showLiveSpinner({
    streaming: liveActive,
    hasDag: !!liveDag,
    answerText: liveTopText,
    visibleActivityCount: orchActivity.length,
  })
  // A DAG turn credits its terminal node; a plain reply credits the orchestrator's top-level run.
  const orchRun = liveTopRuns.find(r => r.runId === 'orchestrator')
  const answerAttribution = liveDag
    ? dagAnswerAttribution(liveDag, liveText)
    : { agent: 'orchestrator', model: orchRun?.model, tokens: orchRun?.totalTokens }
  const hasAnswerBubble = showSpinner || (liveDag ? (!!liveText || !!answerAttribution?.stopped) : (orchActivity.length > 0 || !!liveTopText))
  return { liveDag, liveTopText, liveDone, liveText, orchActivity, showSpinner, answerAttribution, hasAnswerBubble }
}

function LiveDagBubble({ dag, liveDone, chatId, orchActivity, onCancelNode, onPauseNode, onQueueNodeMessage, onEditQueuedMessage, onRemoveQueuedMessage, onEditNodeTask, onRetryNode, onResumeNode, onAnswerNodeQuestion }: {
  dag: DagTurnState
  liveDone: boolean
  chatId?: string
  orchActivity: ReturnType<typeof visibleActivity>
  onCancelNode: (nodeId: string) => void
  onPauseNode: (nodeId: string) => void
  onQueueNodeMessage: (nodeId: string, text: string) => void
  onEditQueuedMessage: (nodeId: string, messageId: string, text: string) => void
  onRemoveQueuedMessage: (nodeId: string, messageId: string) => void
  onEditNodeTask: (nodeId: string, task: string) => void
  onRetryNode: (nodeId: string, guidance?: string) => void
  onResumeNode: (nodeId: string) => void
  onAnswerNodeQuestion: (nodeId: string, answer: string) => void
}) {
  return (
    <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-2xl rounded-tl-sm px-5 py-4">
      <DagBubbleHeader dag={dag} />
      {liveDone ? (
        <details className="rounded-lg border border-gray-200 dark:border-gray-700">
          <summary className="cursor-pointer select-none px-3 py-2 text-xs text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300">
            Steps
          </summary>
          <div className="p-2 space-y-3">
            {orchActivity.length > 0 && <ActivityList activity={orchActivity} />}
            {/* Start/Stop stay wired post-run: a paused node ends the turn, so this is where Start must work. */}
            <DagView dag={dag} chatId={chatId} onRetryNode={onRetryNode} onResumeNode={onResumeNode} onCancelNode={onCancelNode} onAnswerNodeQuestion={onAnswerNodeQuestion} />
          </div>
        </details>
      ) : (
        <div className="space-y-3">
          {orchActivity.length > 0 && <LiveStatusLine activity={orchActivity} />}
          <DagView
            dag={dag}
            chatId={chatId}
            onCancelNode={onCancelNode}
            onPauseNode={onPauseNode}
            onQueueNodeMessage={onQueueNodeMessage}
            onEditQueuedMessage={onEditQueuedMessage}
            onRemoveQueuedMessage={onRemoveQueuedMessage}
            onEditNodeTask={onEditNodeTask}
            onAnswerNodeQuestion={onAnswerNodeQuestion}
          />
        </div>
      )}
    </div>
  )
}

function LiveAnswerBubble({ showSpinner, liveDag, liveText, liveTopText, liveActive, orchActivity, answerAttribution }: {
  showSpinner: boolean
  liveDag?: DagTurnState
  liveText: string
  liveTopText: string
  liveActive: boolean
  orchActivity: ReturnType<typeof visibleActivity>
  answerAttribution: { agent?: string; model?: string; tokens?: number; stopped?: boolean } | undefined
}) {
  return (
    <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-2xl rounded-tl-sm px-5 py-4">
      {showSpinner ? (
        <Dots className="h-5" size="w-2 h-2" />
      ) : liveDag ? (
        <LiveDagAnswer liveText={liveText} liveActive={liveActive} attribution={answerAttribution} />
      ) : (
        // No DAG: orchestrator answered directly (conversational or
        // tool-based research where DAG events don't reach the frontend).
        <div>
          <BubbleHeader
            agent="orchestrator"
            model={answerAttribution?.model}
            tokens={answerAttribution?.tokens}
            status={liveActive ? 'running' : 'done'}
          />
          {orchActivity.length > 0 && (
            liveActive ? <LiveStatusLine activity={orchActivity} /> : <ActivityList activity={orchActivity} />
          )}
          {/* The header's pulsing StatusDot conveys running; no separate spinner while text streams. */}
          {liveTopText && <AssistantText text={liveTopText} streaming={liveActive} />}
        </div>
      )}
    </div>
  )
}

// LiveDagAnswer is the DAG turn's answer: the terminal node's streamed text, or for a node the
// user stopped, its draft badged "not reviewed" (or a bare "Stopped" marker without one).
function LiveDagAnswer({ liveText, liveActive, attribution }: { liveText: string; liveActive: boolean; attribution: { agent?: string; model?: string; tokens?: number; stopped?: boolean } | undefined }) {
  if (!liveText && !attribution?.stopped) return null
  return (
    <>
      <BubbleHeader agent={attribution?.agent ?? 'orchestrator'} model={attribution?.model} tokens={attribution?.tokens} stopped={stoppedBadge(attribution?.stopped, liveText)} />
      {liveText && <AssistantText text={liveText} streaming={liveActive} />}
    </>
  )
}

// The user bubble is hidden for a clarification answer and for a webhook-triggered turn, which has no typed
// message (its synthesized task renders in the DAG bubble).
function LiveTurnView({ live, surfaces, liveActive, isArchived, activeChatId, liveIsChoiceAnswer, livePriorContents, liveAttachmentsEl, liveAttachmentPreviews, submittingChoice, copied, turnsCount, onChoice, onCopy, onDownload, onCancelNode, onPauseNode, onQueueNodeMessage, onEditQueuedMessage, onRemoveQueuedMessage, onEditNodeTask, onRetryNode, onResumeNode, onAnswerNode }: {
  live: NonNullable<ChatState['live']>
  surfaces?: SurfaceRef[]
  liveActive: boolean
  isArchived: boolean
  activeChatId: string | null
  liveIsChoiceAnswer: boolean
  livePriorContents: string[]
  liveAttachmentsEl: React.ReactNode
  liveAttachmentPreviews: { url: string; mime: string; name: string }[]
  submittingChoice: boolean
  copied: string | null
  turnsCount: number
  onChoice: (option: string) => void
  onCopy: (key: string, text: string) => void
  onDownload: (text: string, idx: number) => void
  onCancelNode: (nodeId: string) => void
  onPauseNode: (nodeId: string) => void
  onQueueNodeMessage: (nodeId: string, text: string) => void
  onEditQueuedMessage: (nodeId: string, messageId: string, text: string) => void
  onRemoveQueuedMessage: (nodeId: string, messageId: string) => void
  onEditNodeTask: (nodeId: string, task: string) => void
  onRetryNode: (nodeId: string, guidance?: string) => void
  onResumeNode: (nodeId: string) => void
  onAnswerNode: (nodeId: string, answer: string) => void
}) {
  const d = liveTurnDerived(live, liveActive)
  const { choice, nodeQuestion } = livePendingQuestions(live, d.liveDag, d.liveDone, isArchived)
  const copyKey = `live-${live.userText.slice(0, 20)}`
  return (
    // aria-atomic=false: screen readers announce only the newly streamed text, not the whole region.
    <div key="live" role="log" aria-live="polite" aria-atomic="false">
      {!liveIsChoiceAnswer && (live.userText || liveAttachmentPreviews.length > 0) && (
        <TriggerMessage
          content={live.userText}
          attachments={liveAttachmentsEl}
          priorContents={livePriorContents}
          chatId={activeChatId ?? undefined}
        />
      )}
      <div className="flex justify-start">
        <div className={d.liveDag ? 'w-full space-y-3' : 'w-auto space-y-3'}>
          {d.liveDag && (
            <LiveDagBubble
              dag={d.liveDag}
              liveDone={d.liveDone}
              chatId={activeChatId ?? undefined}
              orchActivity={d.orchActivity}
              onCancelNode={onCancelNode}
              onPauseNode={onPauseNode}
              onQueueNodeMessage={onQueueNodeMessage}
              onEditQueuedMessage={onEditQueuedMessage}
              onRemoveQueuedMessage={onRemoveQueuedMessage}
              onEditNodeTask={onEditNodeTask}
              onRetryNode={onRetryNode}
              onResumeNode={onResumeNode}
              onAnswerNodeQuestion={onAnswerNode}
            />
          )}
          {nodeQuestion && (
            <QuestionBubble
              agent={nodeQuestion.agent}
              question={nodeQuestion.question}
              disabled={submittingChoice}
              onSelect={answer => onAnswerNode(nodeQuestion.nodeId, answer)}
            />
          )}
          {d.hasAnswerBubble && (
            <LiveAnswerBubble
              showSpinner={d.showSpinner}
              liveDag={d.liveDag}
              liveText={d.liveText}
              liveTopText={d.liveTopText}
              liveActive={liveActive}
              orchActivity={d.orchActivity}
              answerAttribution={d.answerAttribution}
            />
          )}
          {choice && (
            <QuestionBubble
              agent="orchestrator"
              question={choice.question}
              options={choice.options}
              disabled={submittingChoice}
              onSelect={onChoice}
            />
          )}
          {d.liveText && (!liveActive) && (
            <div className="flex items-center gap-3 mt-1.5 px-1">
              <button
                onClick={() => onCopy(copyKey, d.liveText)}
                className="min-h-[44px] -my-2 inline-flex items-center text-xs text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 transition-colors"
              >
                {copied === copyKey ? 'Copied!' : 'Copy'}
              </button>
              <button
                onClick={() => onDownload(d.liveText, turnsCount)}
                className="min-h-[44px] -my-2 inline-flex items-center text-xs text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 transition-colors"
              >
                Download
              </button>
            </div>
          )}
        </div>
      </div>
      <TurnSurfaces chatId={activeChatId ?? undefined} surfaces={surfaces} />
    </div>
  )
}

// The sidebar poll can lag the stream, so an 'idle' summary while the turn streams still means running.
function chatHeaderProps(activeChat: ChatSummary | undefined, activeChatId: string | null, isArchived: boolean, live: NonNullable<ChatState['live']> | undefined) {
  return {
    title: activeChat?.title || (activeChatId ? 'New chat' : 'Chat'),
    editable: !!activeChatId && !isArchived,
    status: (activeChat?.status && activeChat.status !== 'idle' ? activeChat.status : 'running') as ChatStatus,
    startedAt: live?.dag?.startedAt ?? live?.runs[0]?.startedAt,
  }
}

function ChatHeader({ title, editable, status, startedAt, activeChatId, isArchived, githubLink, liveActive, onRename, usage, navOpen, onToggleNav, onToggleChatList }: {
  activeChatId: string | null
  isArchived: boolean
  githubLink: { url: string; repo?: string } | null
  liveActive: boolean
  onRename: (title: string) => void
  usage: UsageSummaryProps
  navOpen: boolean
  onToggleNav: () => void
  onToggleChatList: () => void
} & ReturnType<typeof chatHeaderProps>) {
  return (
    <div className="flex items-center justify-between px-4 py-3 sm:px-6 border-b border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800">
      <div className="flex items-center gap-2 min-w-0 flex-1">
        {/* A chat glyph, not a hamburger: two abstract menu icons beside the nav toggle were indistinguishable. */}
        <button
          onClick={onToggleChatList}
          className="medium:hidden flex-shrink-0 w-11 h-11 flex items-center justify-center rounded text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors"
          aria-label="Toggle chat list"
          title="Chats"
        >
          <Icon name="chat" className="w-5 h-5" />
        </button>
        {/* Visible at all widths, unlike the medium:hidden chat-list button. */}
        <NavToggle open={navOpen} onToggle={onToggleNav} />
        {/* min-w-0 lets the title shrink to its flex-1 share instead of overflowing the row, so `truncate`
            clips to as much as fits, never to a few characters. */}
        <div className="min-w-0 flex-1 flex items-center gap-1.5">
          <EditableChatTitle
            title={title}
            editable={editable}
            onRename={onRename}
          />
          {isArchived && (
            <span
              title="This chat is archived and read-only. Restore it from the Archived section to continue."
              className={`flex-shrink-0 text-[11px] font-semibold tracking-wide px-1.5 py-0.5 rounded ${TONE.gray}`}
            >
              Archived
            </span>
          )}
          {githubLink && <GitHubLink url={githubLink.url} repo={githubLink.repo} className="flex-shrink-0" />}
          {liveActive && (
            <ChatHeaderStatus
              status={status}
              startedAt={startedAt}
            />
          )}
        </div>
      </div>
      <div className="flex items-center gap-3 flex-shrink-0">
        {/* Hidden below medium so the title keeps its space; the ⋯ menu still shows usage there. */}
        {activeChatId && (
          <div className="hidden medium:flex">
            <UsageSummary models={usage.models} usage={usage.usage} />
          </div>
        )}
        {activeChatId && (
          <ChatMenu chatId={activeChatId} usage={usage} />
        )}
      </div>
    </div>
  )
}

function showEmptyPrompt(activeChatId: string | null, state: ChatState, live: NonNullable<ChatState['live']> | undefined): boolean {
  return !!activeChatId && state.turns.length === 0 && !live && !state.submitting
}

// Bridges submit to the first stream event; the old `live` still renders above it, so nothing blinks out.
function showPendingTurn(state: ChatState): boolean {
  return !!state.submitting && state.pendingUserText != null
}

// True when the turn asked a get_user_choice clarification, making the next turn's input its answer.
function isChoiceAnswerTurn(turn: Turn | undefined): boolean {
  return turn ? pendingChoice(activityFromTurn(turn)) != null : false
}

// A choice only appears once done (a streaming turn can't pause itself); archived chats are read-only, so
// neither question is offered there.
function livePendingQuestions(live: NonNullable<ChatState['live']>, liveDag: DagTurnState | undefined, liveDone: boolean, isArchived: boolean) {
  const runs = live.runs ?? []
  return {
    choice: liveDone && !isArchived ? pendingChoice(runs) : null,
    // A paused node pauses the whole turn, so liveDone is implied.
    nodeQuestion: liveDag && !isArchived ? pendingNodeQuestion(liveDag) : undefined,
  }
}

// One status-scoped, server-paginated sidebar list. The page token is opaque: only ever
// passed back verbatim, and undefined once the last page has loaded.
function usePagedChats(status: 'active' | 'archived', setItems: (update: (prev: ChatSummary[] | undefined) => ChatSummary[]) => void) {
  const [nextPageToken, setNextPageToken] = useState<string | undefined>(undefined)
  const [loadingMore, setLoadingMore] = useState(false)
  const load = useCallback(async () => {
    const result = await api.listChats({ status: [status] })
    setItems(() => result.data)
    setNextPageToken(result.next_page_token)
    return result.data
  }, [status, setItems])
  const loadMore = useCallback(async () => {
    if (!nextPageToken || loadingMore) return
    setLoadingMore(true)
    try {
      const result = await api.listChats({ status: [status], page_token: nextPageToken })
      setItems(prev => [...(prev ?? []), ...result.data])
      setNextPageToken(result.next_page_token)
    } finally {
      setLoadingMore(false)
    }
  }, [status, setItems, nextPageToken, loadingMore])
  return { hasMore: nextPageToken !== undefined, loadingMore, load, loadMore }
}

export default function Chat({ navOpen, onToggleNav }: ChatProps) {
  const urlChatId = useChatId()

  const store = useChatStore()
  // Server-scoped to status=active: never carries archived rows.
  const [chats, setChats] = useState<ChatSummary[]>([])
  const activePager = usePagedChats('active', setChats)
  // undefined until the Archived section is first expanded, so the initial load never fetches it.
  const [archivedChats, setArchivedChats] = useState<ChatSummary[] | undefined>(undefined)
  const archivedPager = usePagedChats('archived', setArchivedChats)
  // Optimistically archived ids: keeps a stale in-flight poll from resurrecting one.
  const archivingIdsRef = useRef<Set<string>>(new Set())
  const [activeChatId, setActiveChatId] = useState<string | null>(null)
  // Gates the status-triggered re-attach until seed() ran: the poll can mark a chat running before getChat
  // resolves, and an early attach() makes seed() a no-op, dropping history.
  const [seededChatId, setSeededChatId] = useState<string | null>(null)
  // An archived focused chat, kept out of the active-scoped `chats` list.
  const [activeChatDetail, setActiveChatDetail] = useState<ChatSummary | null>(null)
  const activeChat = resolveActiveChat(chats, activeChatId, activeChatDetail)
  const githubLink = chatGitHubLink(activeChat)
  const state = useChatState(activeChatId)
  const streaming = !!state.live?.streaming
  // An archived chat's focused view is read-only: it never presents as active,
  // even if a run left running through the archive (backend leaves those alone).
  const isArchived = !!activeChat?.archived
  const liveActive = streaming && !isArchived
  const error = state.error
  const live = state.live
  const [chatListOpen, setChatListOpen] = useState(false)
  const [copied, setCopied] = useState<string | null>(null)
  // A failed first-send chat creation has no chat yet to attach state.error to.
  const [createChatError, setCreateChatError] = useState('')
  useEffect(() => { setCreateChatError('') }, [activeChatId])
  const [submittingChoice, setSubmittingChoice] = useState(false)
  const [liveAttachmentPreviews, setLiveAttachmentPreviews] = useState<{url: string; mime: string; name: string}[]>([])
  // Stable element identity so memo(TriggerMessage) actually skips re-renders;
  // a fresh <AttachmentPreviews> per render defeats memo even when props are equal.
  const liveAttachmentsEl = useMemo(
    () => <AttachmentPreviews previews={liveAttachmentPreviews} />,
    [liveAttachmentPreviews],
  )
  const scrollRef = useRef<HTMLDivElement>(null)

  // Keyed on turn count so it fires after the async seed lands; deliberately doesn't follow mid-stream tokens.
  useLayoutEffect(() => {
    const el = scrollRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [activeChatId, state.turns.length])

  const loadChats = activePager.load
  const loadArchivedChats = archivedPager.load

  // archivedChats stays undefined until the first expand, so only that expand fetches.
  const handleExpandArchived = useCallback(() => {
    if (archivedChats === undefined) void loadArchivedChats()
  }, [archivedChats, loadArchivedChats])

  // chats and archivedChats are disjoint server-scoped lists, so archiving moves a chat between them.
  const handleArchiveChat = useCallback(async (chatId: string) => {
    const existing = chats.find(c => c.id === chatId) ?? archivedChats?.find(c => c.id === chatId)
    if (!existing) return
    const newArchived = !existing.archived

    function move(item: ChatSummary, archived: boolean) {
      if (archived) {
        setChats(prev => prev.filter(c => c.id !== chatId))
      } else {
        setChats(prev => [item, ...prev])
      }
      setArchivedChats(prev => nextArchivedChats(prev, item, archived))
      // The header and composer fall back to this snapshot once the chat leaves `chats`, so it must flip too.
      setActiveChatDetail(prev => (prev?.id === chatId ? { ...prev, archived } : prev))
    }
    if (newArchived) archivingIdsRef.current.add(chatId)
    move({ ...existing, archived: newArchived }, newArchived)

    try {
      await api.archiveChat(chatId, newArchived)
    } catch {
      move(existing, !newArchived) // revert to the original scope
    } finally {
      if (newArchived) archivingIdsRef.current.delete(chatId)
    }
  }, [chats, archivedChats])

  useEffect(() => {
    void loadChats().then(data => {
      if (urlChatId) {
        setActiveChatId(urlChatId)
      } else if (data.length > 0) {
        setActiveChatId(data[0].id)
        navigate(`/chat/${data[0].id}`, { replace: true })
      }
    })
  }, [])

  useEffect(() => {
    if (!activeChatId) return
    let cancelled = false
    api.getChat(activeChatId).then(detail => {
      if (cancelled) return
      setActiveChatDetail(detail)
      // An archived chat never joins the active-scoped list, however it was reached.
      if (!detail.archived) {
        setChats(prev => {
          const exists = prev.find(s => s.id === activeChatId)
          if (exists) return prev
          return [detail, ...prev]
        })
      }
      store.seed(activeChatId, detail.turns, detail.usage)
      setSeededChatId(activeChatId)
      // A finished DAG turn attaches too: replaying chat_events is the only record of judge/revise sub-run cards,
      // which node_states doesn't break down per run.
      const lastTurn = detail.turns[detail.turns.length - 1]
      if (!detail.archived && (detail.status === 'running' || (lastTurn && dagFromTurn(lastTurn) != null))) {
        store.attach(activeChatId)
      }
    }).catch(() => {})
    return () => {
      cancelled = true
      store.detachStream(activeChatId)
    }
  }, [activeChatId])

  const { turnImages, turnSurfaces } = useTurnArtifacts(activeChatId, state)

  // A webhook-triggered run has no client action to hang off, so the sidebar polls rather than pushing on navigate.
  useEffect(() => {
    let cancelled = false
    async function doPoll() {
      try {
        // First page only: re-requesting the loaded count would come back short above the server's page cap.
        // Anything just changed sorts into this page; the Archived section isn't live-polled.
        const result = await api.listChats({ status: ['active'] })
        if (!cancelled) setChats(prev => mergeChatsPage(prev, pollPageExcludingPending(result.data, archivingIdsRef.current)))
      } catch { /* transient - next poll will retry */ }
    }
    void loadChats().then(data => { if (!cancelled) setChats(data) })
    const stop = pollWhileVisible(() => { void doPoll() }, 5000)
    return () => {
      cancelled = true
      stop()
    }
  }, [loadChats])

  // A run started elsewhere (webhook, CLI retry) on the open chat re-seeds and attaches so its turn goes live.
  useEffect(() => {
    if (!activeChatId || !activeChat?.status || activeChat.archived) return
    if (seededChatId !== activeChatId) return // wait for the getChat effect's own attach - see seededChatId above
    const s = activeChat.status
    if (s !== 'running' || store.isStreaming(activeChatId)) return
    void api.getChat(activeChatId).then(detail => store.reattach(activeChatId, detail.turns)).catch(() => {})
  }, [activeChatId, activeChat?.status, activeChat?.archived, seededChatId])

  function activateChat(id: string) {
    setActiveChatId(id)
    navigate(`/chat/${id}`)
  }

  const handleRenameChat = useCallback(async (title: string) => {
    if (!activeChatId) return
    const updated = await api.renameChat(activeChatId, title)
    setChats(prev => prev.map(c => c.id === activeChatId ? updated : c))
  }, [activeChatId])

  async function handleNewChat() {
    const chat = await api.createChat()
    setChats(prev => [chat, ...prev])
    setActiveChatId(chat.id)
    navigate(`/chat/${chat.id}`)
  }

  async function handleDeleteChat(id: string, e: React.MouseEvent) {
    e.stopPropagation()
    store.stop(id)
    await api.deleteChat(id)
    store.clear(id)
    setChats(prev => prev.filter(s => s.id !== id))
    setArchivedChats(prev => prev?.filter(s => s.id !== id))
    if (activeChatId === id) {
      const remaining = chats.filter(s => s.id !== id)
      if (remaining.length > 0) {
        setActiveChatId(remaining[0].id)
        navigate(`/chat/${remaining[0].id}`)
      } else {
        setActiveChatId(null)
        navigate('/chat')
      }
    }
  }

  // Stable identity, or every memoized TurnView re-renders on each parent render.
  const handleCopy = useCallback((key: string, content: string) => {
    void navigator.clipboard.writeText(content)
    setCopied(key)
    setTimeout(() => setCopied(null), 2000)
  }, [])

  const handleDownload = useCallback((content: string, idx: number) => {
    const h1 = content.match(/^#\s+(.+)$/m)?.[1]?.trim()
    const slug = h1 ? h1.replace(/[^\w\s-]/g, '').trim().replace(/\s+/g, '-') : `answer-${idx + 1}`
    const blob = new Blob([content], { type: 'text/markdown' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `${slug}.md`
    a.click()
    URL.revokeObjectURL(url)
  }, [])

  const handleStop = useCallback(() => {
    if (activeChatId) store.stop(activeChatId)
  }, [activeChatId, store])

  const handleCancelNode = useCallback((nodeId: string) => {
    if (activeChatId) store.stopNode(activeChatId, nodeId)
  }, [activeChatId, store])

  const handlePauseNode = useCallback((nodeId: string) => {
    if (activeChatId) store.pauseNode(activeChatId, nodeId)
  }, [activeChatId, store])

  const handleResumeNode = useCallback((nodeId: string) => {
    if (activeChatId) store.startNode(activeChatId, nodeId)
  }, [activeChatId, store])

  const handleQueueNodeMessage = useCallback((nodeId: string, text: string) => {
    if (activeChatId) void store.queueNodeMessage(activeChatId, nodeId, text)
  }, [activeChatId, store])

  const handleEditQueuedMessage = useCallback((nodeId: string, messageId: string, text: string) => {
    if (activeChatId) void store.editQueuedMessage(activeChatId, nodeId, messageId, text)
  }, [activeChatId, store])

  const handleRemoveQueuedMessage = useCallback((nodeId: string, messageId: string) => {
    if (activeChatId) void store.removeQueuedMessage(activeChatId, nodeId, messageId)
  }, [activeChatId, store])

  const handleEditNodeTask = useCallback((nodeId: string, task: string) => {
    if (activeChatId) void store.editNodeTask(activeChatId, nodeId, task)
  }, [activeChatId, store])

  const handleRetryNode = useCallback((nodeId: string, guidance?: string) => {
    if (activeChatId) store.retryNode(activeChatId, nodeId, guidance)
  }, [activeChatId, store])

  // The answer travels as NodeStartBody.content, the same delivery every paused/awaiting_input resume uses.
  const handleAnswerNode = useCallback((nodeId: string, answer: string) => {
    if (!activeChatId) return
    store.startNode(activeChatId, nodeId, answer)
  }, [activeChatId, store])

  // Two rapid sends before createChat() resolves both see activeChatId null; sharing one promise lands
  // the second send in the chat the first created.
  const creatingChatRef = useRef<Promise<string> | null>(null)

  // A failed chat creation rethrows so the Composer restores the unsent draft. The queue check reads the store,
  // not the render-time `streaming`, since a just-created chat's turn can start before this re-renders.
  const submitMessage = useCallback(async (text: string, files: File[], previews: { url: string; mime: string; name: string }[]) => {
    let chatId = activeChatId
    if (!chatId) {
      if (!creatingChatRef.current) {
        creatingChatRef.current = api.createChat()
          .then(chat => {
            setChats(prev => [chat, ...prev])
            setActiveChatId(chat.id)
            navigate(`/chat/${chat.id}`)
            return chat.id
          })
          .finally(() => { creatingChatRef.current = null })
      }
      try {
        chatId = await creatingChatRef.current
      } catch (err) {
        setCreateChatError(err instanceof Error ? err.message : 'Failed to start a new chat')
        throw err
      }
    }
    if (store.get(chatId).live?.streaming) {
      store.queueTurn(chatId, text)
      return
    }
    setLiveAttachmentPreviews(previews)
    void store.submit(chatId, text, files.length > 0 ? files : undefined, title => {
      setChats(prev => prev.map(c => c.id === chatId ? { ...c, title } : c))
    }).then(() => loadChats().then(data => setChats(data)))
  }, [activeChatId, store, loadChats])

  const handleRemoveQueued = useCallback((id: string) => {
    if (activeChatId) store.unqueueTurn(activeChatId, id)
  }, [activeChatId, store])

  // The backend resumes the next message as the tool's answer. The local guard blocks a double-send before
  // store.submit flips the streaming flag.
  const handleChoice = useCallback(async (option: string) => {
    if (!activeChatId || submittingChoice) return
    setSubmittingChoice(true)
    try {
      await store.submit(activeChatId, option, undefined, title => {
        setChats(prev => prev.map(c => c.id === activeChatId ? { ...c, title } : c))
      })
      await loadChats().then(data => setChats(data))
    } finally {
      setSubmittingChoice(false)
    }
  }, [activeChatId, submittingChoice, store, loadChats])

  const selectChat = useCallback((id: string) => { activateChat(id); setChatListOpen(false) }, [])

  // state.turns keeps a stable ref while only `live` changes, so streaming tokens don't re-parse every turn.
  const liveUserText = live?.userText
  const turnViews = useMemo(() => state.turns.map((turn, idx, arr) => {
    const turnChoice = pendingChoice(activityFromTurn(turn))
    // For the last turn the answer is the live turn's input; undefined means still answerable.
    const next = arr[idx + 1]
    const choiceAnswer = turnChoice ? (next ? next.input.content : liveUserText) : undefined
    const prev = arr[idx - 1]
    const isChoiceAnswer = prev ? pendingChoice(activityFromTurn(prev)) != null : false
    // Lets TriggerMessage fold a GitHub <comments> delta onto the running history.
    const priorContents = arr.slice(0, idx).map(t => t.input.content)
    return { turn, idx, choiceAnswer, isChoiceAnswer, priorContents, imageAttachments: turnImages[turn.id], surfaces: turnSurfaces[turn.id] }
  }), [state.turns, liveUserText, turnImages, turnSurfaces])

  // Unbounded, 2,000 turns took 5.9 s to first paint. Slicing after turnViews keeps idx/priorContents/choiceAnswer
  // on the full history; resets per chat.
  const [visibleTurnCount, setVisibleTurnCount] = useState(RENDERED_TURN_TAIL)
  useEffect(() => { setVisibleTurnCount(RENDERED_TURN_TAIL) }, [activeChatId])
  const mountedTurnViews = useMemo(() => turnViews.slice(-visibleTurnCount), [turnViews, visibleTurnCount])
  const hiddenTurnCount = turnViews.length - mountedTurnViews.length

  const liveIsChoiceAnswer = isChoiceAnswerTurn(state.turns[state.turns.length - 1])

  const livePriorContents = useMemo(() => state.turns.map(t => t.input.content), [state.turns])

  return (
    // The app owns all scrolling; without overflow-hidden an over-tall child scrolls the page past the composer.
    <div className="flex h-full overflow-hidden bg-gray-50 dark:bg-gray-900 text-gray-900 dark:text-white">
      <ChatList
        chats={chats}
        activeChatId={activeChatId}
        open={chatListOpen}
        onSelect={selectChat}
        onNewChat={() => { void handleNewChat() }}
        onDelete={(id, e) => { void handleDeleteChat(id, e) }}
        onCloseMobile={() => setChatListOpen(false)}
        hasMoreChats={activePager.hasMore}
        onLoadMoreChats={() => { void activePager.loadMore() }}
        loadingMoreChats={activePager.loadingMore}
        // Safe to share: handleArchiveChat toggles off current state, and ChatRow
        // only fires onArchive from an active row / onUnarchive from an archived one.
        onArchive={(chatId) => { void handleArchiveChat(chatId) }}
        onUnarchive={(chatId) => { void handleArchiveChat(chatId) }}
        archivedChats={archivedChats}
        hasMoreArchivedChats={archivedPager.hasMore}
        onLoadMoreArchivedChats={() => { void archivedPager.loadMore() }}
        loadingMoreArchivedChats={archivedPager.loadingMore}
        onExpandArchived={handleExpandArchived}
      />

      <div className="flex flex-col flex-1 min-w-0">
        <ChatHeader
          {...chatHeaderProps(activeChat, activeChatId, isArchived, live)}
          activeChatId={activeChatId}
          isArchived={isArchived}
          githubLink={githubLink}
          liveActive={liveActive}
          onRename={(title) => { void handleRenameChat(title) }}
          usage={{ models: sessionModels(state), usage: state.usage }}
          navOpen={navOpen}
          onToggleNav={onToggleNav}
          onToggleChatList={() => setChatListOpen(o => !o)}
        />

        {/* The composer floats over the list; the scroll pane's bottom padding keeps the last message clear. */}
        <div className="relative flex-1 min-h-0">
        {/* Clearance is composer height plus --composer-gap, so the inset grows it instead of counting twice. */}
        <div ref={scrollRef} className="absolute inset-0 overflow-y-auto overscroll-contain px-6 pt-6 pb-[calc(7rem+var(--composer-gap))] medium:pb-[calc(8rem+var(--composer-gap))] space-y-6">
          {showEmptyPrompt(activeChatId, state, live) && (
            <div className="text-center text-gray-500 dark:text-gray-400 text-sm mt-20">
              Ask a question
            </div>
          )}

          {hiddenTurnCount > 0 && (
            <button
              onClick={() => setVisibleTurnCount(c => c + RENDERED_TURN_TAIL)}
              className="w-full min-h-[44px] text-xs text-center text-blue-600 dark:text-blue-400 hover:bg-gray-100 dark:hover:bg-gray-800 rounded-lg transition-colors"
            >
              Show {Math.min(hiddenTurnCount, RENDERED_TURN_TAIL)} older messages
            </button>
          )}

          {mountedTurnViews.map(({ turn, idx, choiceAnswer, isChoiceAnswer, priorContents, imageAttachments, surfaces }) => (
            <TurnView
              key={turn.id}
              turn={turn}
              idx={idx}
              chatId={activeChatId ?? undefined}
              choiceAnswer={choiceAnswer}
              isChoiceAnswer={isChoiceAnswer}
              submittingChoice={submittingChoice}
              isCopied={copied === `turn-${turn.id}`}
              priorContents={priorContents}
              imageAttachments={imageAttachments}
              surfaces={surfaces}
              onChoice={(option) => { void handleChoice(option) }}
              onCopy={handleCopy}
              onDownload={(content, idx) => { void handleDownload(content, idx) }}
            />
          ))}

          {live && (
            <LiveTurnView
              live={live}
              surfaces={turnSurfaces[live.id]}
              liveActive={liveActive}
              isArchived={isArchived}
              activeChatId={activeChatId}
              liveIsChoiceAnswer={liveIsChoiceAnswer}
              livePriorContents={livePriorContents}
              liveAttachmentsEl={liveAttachmentsEl}
              liveAttachmentPreviews={liveAttachmentPreviews}
              submittingChoice={submittingChoice}
              copied={copied}
              turnsCount={state.turns.length}
              onChoice={(option) => { void handleChoice(option) }}
              onCopy={handleCopy}
              onDownload={(content, idx) => { void handleDownload(content, idx) }}
              onCancelNode={handleCancelNode}
              onPauseNode={handlePauseNode}
              onQueueNodeMessage={handleQueueNodeMessage}
              onEditQueuedMessage={handleEditQueuedMessage}
              onRemoveQueuedMessage={handleRemoveQueuedMessage}
              onEditNodeTask={handleEditNodeTask}
              onRetryNode={handleRetryNode}
              onResumeNode={handleResumeNode}
              onAnswerNode={handleAnswerNode}
            />
          )}

          {showPendingTurn(state) && (
            <div>
              <TriggerMessage content={state.pendingUserText!} />
              <div className="flex justify-start">
                <div className="w-auto">
                  <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-2xl rounded-tl-sm px-5 py-4" role="status" aria-label="Thinking">
                    <Dots className="h-5" size="w-2 h-2" />
                  </div>
                </div>
              </div>
            </div>
          )}

          {(error || createChatError) && (
            <div className="rounded-md bg-red-50 dark:bg-red-950/30 border border-red-200 dark:border-red-800 px-4 py-3 text-sm text-red-700 dark:text-red-400">
              {error || createChatError}
            </div>
          )}
        </div>

        <div className="absolute inset-x-0 bottom-0">
          <Composer
            disabled={isArchived}
            streaming={liveActive}
            onSubmit={submitMessage}
            onStop={handleStop}
            queue={state.queue}
            onRemoveQueued={handleRemoveQueued}
            archived={isArchived}
            noChat={!activeChatId}
          />
        </div>
        </div>
      </div>
    </div>
  )
}
