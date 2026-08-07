import { useState, useEffect, useLayoutEffect, useRef, useCallback, useMemo } from 'react'
import { navigate, useChatId } from '../router'
import { api, type ChatSummary } from '../api'
import { AssistantText, ActivityList, LiveStatusLine, BubbleHeader, Dots } from '../components/AgentParts'
import { QuestionBubble } from '../components/QuestionBubble'
import { DagView, DagBubbleHeader } from '../components/DagView'
import { Composer } from '../components/Composer'
import { ChatList } from '../components/ChatList'
import { TurnView, visibleActivity } from '../components/TurnView'
import { useChatStore, useChatState } from '../state/ChatStoreProvider'
import { activityFromTurn, isTurnInProgress, terminalNodeId, pendingNodeQuestion, dagAnswerAttribution, type DagTurnState } from '../state/chatStore'
import { pendingChoice, showLiveSpinner } from '../components/messageParts'
import { AttachmentPreviews } from '../components/AttachmentUI'
import { GitHubLink } from '../components/GitHubLink'
import { TriggerMessage } from '../components/TriggerEnvelope'
import { ChatMenu } from '../components/ChatMenu'

// liveDagFinalText extracts the answer from the terminal node's accumulated answer.
// This IS the DAG turn's answer - never mix in the orchestrator's own top-level
// text (that's planning/narration chatter, not the reply; see liveText below).
export function liveDagFinalText(dag: DagTurnState): string {
  const finalId = terminalNodeId(dag.nodes)
  return finalId != null ? (dag.nodeAnswer[finalId] ?? '') : ''
}

// shouldQueueSubmit is the Composer send decision: queue while a run is
// streaming (drained automatically once it finishes), send immediately
// otherwise - the same immediate path as before this feature existed.
export function shouldQueueSubmit(streaming: boolean): boolean {
  return streaming
}

// mergeChatsPage folds a freshly-polled page (server order: most-recently-
// updated first) into the existing list without ever shortening it: a chat
// in `page` replaces its stale copy (or is added if new); everything else in
// `existing` - beyond what a bounded page can cover - is left untouched.
// Safe to concatenate in this order: `page` is exactly the current top N by
// updated_at, so nothing left in `existing` can outrank it (#736 - the poll
// must never truncate a sidebar the user has paged past the page cap).
export function mergeChatsPage(existing: ChatSummary[], page: ChatSummary[]): ChatSummary[] {
  const pageIds = new Set(page.map(c => c.id))
  return [...page, ...existing.filter(c => !pageIds.has(c.id))]
}

// pollWhileVisible calls `poll` on an interval, but only while the document is visible - a
// backgrounded tab has nothing to show, so it costs nothing (#738). Becoming visible again
// fires `poll` immediately rather than waiting out the rest of the interval. Returns a
// cleanup function that stops the interval and removes the listener.
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

// chatGitHubLink (#382) extracts the header's back-link target from a chat's
// summary: present only for a GitHub-originated chat (github_url set by the
// webhook at dispatch time), null for a direct chat - so the header renders
// nothing extra for local chats.
export function chatGitHubLink(chat: ChatSummary | undefined): { url: string; repo?: string } | null {
  if (!chat?.github_url) return null
  return { url: chat.github_url, repo: chat.github_repo }
}

export interface EditableChatTitleProps {
  title: string
  editable: boolean
  onRename: (title: string) => void
}

// EditableChatTitle is the header's click-to-edit title: a click (when
// `editable`, i.e. a chat is active) swaps the heading for a text input;
// Enter/blur commits, Escape cancels. `onRename` fires only for an actual
// change - a blank or unchanged draft is a silent no-op, not a rename to ''.
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
      className={`group flex items-center gap-1.5 text-base font-semibold text-gray-900 dark:text-white truncate ${editable ? 'cursor-text' : ''}`}
    >
      <span className="truncate">{title}</span>
      {editable && (
        <span className="opacity-0 group-hover:opacity-100 text-gray-400 text-xs transition-opacity flex-shrink-0" aria-hidden="true">
          ✎
        </span>
      )}
    </h1>
  )
}

export default function Chat() {
  const urlChatId = useChatId()

  const store = useChatStore()
  const [chats, setChats] = useState<ChatSummary[]>([])
  // Opaque continuation token for the next page of chats (#736: the sidebar
  // list is server-paginated). undefined once the last page has loaded;
  // never parsed here, only ever passed back to the server verbatim.
  const [chatsNextPageToken, setChatsNextPageToken] = useState<string | undefined>(undefined)
  const [loadingMoreChats, setLoadingMoreChats] = useState(false)
  const [activeChatId, setActiveChatId] = useState<string | null>(null)
  // Set once GET /chats/{id} has seeded this chat's turns - gates the status-
  // triggered re-attach below so it can never fire ahead of seed() (#463: the
  // sidebar's poll can mark a chat 'running' before its own getChat resolves,
  // and an early attach() makes seed() a no-op, permanently dropping history).
  const [seededChatId, setSeededChatId] = useState<string | null>(null)
  const activeChat = chats.find(s => s.id === activeChatId)
  const githubLink = chatGitHubLink(activeChat)
  const state = useChatState(activeChatId)
  const streaming = state.live?.streaming ?? false
  const error = state.error
  const live = state.live
  const [chatListOpen, setChatListOpen] = useState(false)
  const [copied, setCopied] = useState<string | null>(null)
  const [submittingChoice, setSubmittingChoice] = useState(false)
  const [liveAttachmentPreviews, setLiveAttachmentPreviews] = useState<{url: string; mime: string; name: string}[]>([])
  // #722: control whether archived chats are included in the sidebar list.
  // Defaults to false — the collapsed Archived section refetches with true on expand.
  const [showArchived, setShowArchived] = useState(false)
  const scrollRef = useRef<HTMLDivElement>(null)

  // Open a chat scrolled to the latest message (and snap down as turns complete),
  // not pinned to the top of a long history. Keyed on turn count so it fires after
  // the async seed lands. Doesn't follow mid-stream tokens (not requested).
  useLayoutEffect(() => {
    const el = scrollRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [activeChatId, state.turns.length])

  const loadChats = useCallback(async () => {
    const result = showArchived
      ? await api.listChatsWithArchived({ show_archived: true })
      : await api.listChats()
    setChats(result.data)
    setChatsNextPageToken(result.next_page_token)
    return result.data
  }, [showArchived])

  const loadMoreChats = useCallback(async () => {
    if (!chatsNextPageToken || loadingMoreChats) return
    setLoadingMoreChats(true)
    try {
      const result = showArchived
        ? await api.listChatsWithArchived({ show_archived: true, page_token: chatsNextPageToken })
        : await api.listChats({ page_token: chatsNextPageToken })
      setChats(prev => [...prev, ...result.data])
      setChatsNextPageToken(result.next_page_token)
    } finally {
      setLoadingMoreChats(false)
    }
  }, [chatsNextPageToken, loadingMoreChats, showArchived])

  // Refetch when the archived expand toggle changes so the list updates
  // with or without archived chats (#722).
  useEffect(() => {
    void loadChats()
  }, [showArchived, loadChats])

  const handleArchiveChat = useCallback(async (chatId: string) => {
    const existing = chats.find(c => c.id === chatId)
    if (!existing) return
    const newArchived = !existing.archived

    setChats(prev => prev.map(c =>
      c.id === chatId ? { ...c, archived: newArchived } : c
    ))

    try {
      await api.archiveChat(chatId, newArchived)
    } catch {
      setChats(prev => prev.map(c =>
        c.id === chatId ? { ...c, archived: !newArchived } : c
      ))
    }
  }, [chats])

  useEffect(() => {
    loadChats().then(data => {
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
      setChats(prev => {
        const exists = prev.find(s => s.id === activeChatId)
        if (exists) return prev
        return [detail, ...prev]
      })
      store.seed(activeChatId, detail.turns)
      setSeededChatId(activeChatId)
      // Reconnect to a run still in progress (e.g. this browser after a refresh):
      // the POST body stream is gone, so subscribe to the hub. attach no-ops if
      // this client already streams (it posted the run) - no double-subscribe.
      // detail.status is the hub's authoritative "a run is live" signal and is
      // true through EVERY phase (queued behind max_active_runs, planning,
      // queued nodes, streaming) - the DAG check alone missed pre-DAG phases (no
      // quack:dag item persisted yet), so a refresh during planning never
      // reconnected. queued is included: the run's hub topic is already live
      // (response_created publishes at admission, before the run slot is
      // acquired), so a refresh while queued must still attach. The DAG check
      // stays as a fallback for a restarted server whose in-memory hub state
      // died with it.
      if (detail.status === 'running' || detail.status === 'queued' || isTurnInProgress(detail.turns[detail.turns.length - 1])) {
        store.attach(activeChatId)
      }
    }).catch(() => {})
    return () => {
      cancelled = true
      store.detachStream(activeChatId)
    }
  }, [activeChatId])

 // #499/#738: poll the chat list so the sidebar stays current without a refresh. Skipped
  // while the tab is hidden (a backgrounded tab has nothing to show anyway) and resumed
  // immediately on refocus rather than waiting out the rest of the interval - a GitHub-webhook-triggered
  // run has no client action to hang off, so this stays a poll, not push-on-navigate.
  useEffect(() => {
    let cancelled = false
    async function doPoll() {
      try {
        // Always just the first (default-size) page, merged into whatever's
        // already loaded - never re-requested at the loaded count, which
        // above the server's page cap would silently come back shorter than
        // what's on screen (#736). Chats are updated_at-sorted, so anything
        // that just changed is in this page regardless of how far the user
        // has paged; the pagination token is left untouched here.
        const result = showArchived
          ? await api.listChatsWithArchived({ show_archived: true })
          : await api.listChats()
        if (!cancelled) setChats(prev => mergeChatsPage(prev, result.data))
      } catch { /* transient - next poll will retry */ }
    }
    loadChats().then(data => { if (!cancelled) setChats(data) })
    const stop = pollWhileVisible(() => { void doPoll() }, 5000)
    return () => {
      cancelled = true
      stop()
    }
  }, [loadChats, showArchived])

  // #463: when a run goes active on an already-open chat (e.g. GitHub webhook
  // dispatched while the user views this chat), re-fire attach so the SSE
  // subscribe stream opens and live events start flowing.  Without it, only
  // ChatList's Running badge lights up - the chat box stays blank because
  // the /stream subscription never started for this client.  attach no-ops if
  // this client already streams (it posted the run) or has an existing
  // EventSource - no double-subscribe.
  useEffect(() => {
    if (!activeChatId || !activeChat?.status) return
    if (seededChatId !== activeChatId) return // wait for the getChat effect's own attach - see seededChatId above
    const s = activeChat.status
    if (s === 'running' || s === 'queued') {
      store.attach(activeChatId)
    }
  }, [activeChatId, activeChat?.status, seededChatId])

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

  // useCallback so the handlers passed to memoized TurnViews keep a stable identity
  // (otherwise every completed turn re-renders on each parent render).
  const handleCopy = useCallback((key: string, content: string) => {
    navigator.clipboard.writeText(content)
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
    if (activeChatId) store.cancelNode(activeChatId, nodeId)
  }, [activeChatId, store])

  const handlePauseNode = useCallback((nodeId: string) => {
    if (activeChatId) store.pauseNode(activeChatId, nodeId)
  }, [activeChatId, store])

  const handleResumeNode = useCallback((nodeId: string) => {
    if (activeChatId) store.resumeNode(activeChatId, nodeId)
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

  // handleAnswerNode answers a paused node's question (mid-node HITL) by sending
  // the answer as the next message - the backend delivers it to the paused node.
  const handleAnswerNode = useCallback((_nodeId: string, answer: string) => {
    if (!activeChatId) return
    void store.submit(activeChatId, answer, undefined, title => {
      setChats(prev => prev.map(c => c.id === activeChatId ? { ...c, title } : c))
    }).then(() => loadChats().then(data => setChats(data)))
  }, [activeChatId, store, loadChats])

  // submitMessage is the Composer's send action. While the chat is streaming
  // it queues instead of starting a second concurrent run (store.drainQueue
  // submits it automatically once the current run finishes); otherwise it
  // sends immediately, same as before.
  const submitMessage = useCallback((text: string, files: File[], previews: { url: string; mime: string; name: string }[]) => {
    if (!activeChatId) return
    if (shouldQueueSubmit(streaming)) {
      store.queueTurn(activeChatId, text)
      return
    }
    setLiveAttachmentPreviews(previews)
    store.submit(activeChatId, text, files.length > 0 ? files : undefined, title => {
      setChats(prev => prev.map(c => c.id === activeChatId ? { ...c, title } : c))
    }).then(() => loadChats().then(data => setChats(data)))
  }, [activeChatId, store, loadChats, streaming])

  const handleRemoveQueued = useCallback((id: string) => {
    if (activeChatId) store.unqueueTurn(activeChatId, id)
  }, [activeChatId, store])

  // handleChoice answers a get_user_choice clarification by sending the chosen
  // option as the next message (the backend resumes it as the tool's answer).
  // Reuses the normal send path. The local guard prevents a double-send during
  // the brief window before store.submit flips the streaming flag.
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

  // Per-turn props for the completed turns. Memoized on [turns, live.userText] so it
  // is NOT recomputed on every streaming token (state.turns keeps a stable ref while
  // only `live` changes) - the key to not re-parsing every turn's markdown mid-stream.
  const liveUserText = live?.userText
  const turnViews = useMemo(() => state.turns.map((turn, idx, arr) => {
    const turnChoice = pendingChoice(activityFromTurn(turn))
    // The answer to a clarification is the next turn's input, or - for the last
    // turn - the live turn's input. Undefined means it's still answerable.
    const next = arr[idx + 1]
    const choiceAnswer = turnChoice ? (next ? next.input.content : liveUserText) : undefined
    // This turn's input is itself the answer to the previous turn's clarification.
    const prev = arr[idx - 1]
    const isChoiceAnswer = prev ? pendingChoice(activityFromTurn(prev)) != null : false
    // Earlier turns' raw envelope text, oldest first - lets this turn's
    // TriggerMessage fold a GitHub <comments> delta onto the running history (#730).
    const priorContents = arr.slice(0, idx).map(t => t.input.content)
    return { turn, idx, choiceAnswer, isChoiceAnswer, priorContents }
  }), [state.turns, liveUserText])

  // The live turn is a clarification answer when the last completed turn asked one.
  const lastTurn = state.turns[state.turns.length - 1]
  const liveIsChoiceAnswer = lastTurn ? pendingChoice(activityFromTurn(lastTurn)) != null : false

  // Every completed turn's raw envelope text, oldest first - the live turn's
  // <comments> section folds onto this the same way a persisted turn does (#730).
  const livePriorContents = useMemo(() => state.turns.map(t => t.input.content), [state.turns])

  return (
    // overflow-hidden: the app owns ALL scrolling internally (messages pane,
    // sidebar list); without it any over-tall child stretches the document and
    // the page itself scrolls into blank space below the composer.
    <div className="flex h-full overflow-hidden bg-gray-50 dark:bg-gray-900 text-gray-900 dark:text-white">
      {chatListOpen && (
        <div
          className="md:hidden fixed inset-0 z-30 bg-black/50"
          onClick={() => setChatListOpen(false)}
          aria-hidden="true"
        />
      )}

      <ChatList
        chats={chats}
        activeChatId={activeChatId}
        open={chatListOpen}
        onSelect={selectChat}
        onNewChat={handleNewChat}
        onDelete={handleDeleteChat}
        onCloseMobile={() => setChatListOpen(false)}
        hasMoreChats={chatsNextPageToken !== undefined}
        onLoadMoreChats={loadMoreChats}
        loadingMoreChats={loadingMoreChats}
        onArchive={handleArchiveChat}
        onUnarchive={handleArchiveChat}
        onShowArchived={() => setShowArchived(true)}
      />

      <div className="flex flex-col flex-1 min-w-0">
        <div className="flex items-center justify-between px-4 py-3 sm:px-6 border-b border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800">
          <div className="flex items-center gap-2 min-w-0">
            <button
              onClick={() => setChatListOpen(o => !o)}
              className="md:hidden flex-shrink-0 w-8 h-8 flex items-center justify-center rounded text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors"
              aria-label="Toggle chat list"
            >
              ☰
            </button>
            <EditableChatTitle
              title={activeChat?.title || (activeChatId ? 'New chat' : 'Chat')}
              editable={!!activeChatId}
              onRename={handleRenameChat}
            />
            {githubLink && <GitHubLink url={githubLink.url} repo={githubLink.repo} className="flex-shrink-0" />}
          </div>
          {/* Per-chat actions (#746 items 2/3): Download Logs is the escape
              hatch to the untrimmed event-by-event recording - relabelled and
              moved from a standing header link into the ⋯ overflow menu. */}
          {activeChatId && <ChatMenu chatId={activeChatId} />}
        </div>

        <div ref={scrollRef} className="flex-1 overflow-y-auto overscroll-contain px-6 py-6 space-y-6">
          {!activeChatId && (
            <div className="text-center text-gray-400 dark:text-gray-500 text-sm mt-20">
              Select or start a chat
            </div>
          )}
          {activeChatId && state.turns.length === 0 && !live && !state.submitting && (
            <div className="text-center text-gray-400 dark:text-gray-500 text-sm mt-20">
              Ask a question
            </div>
          )}

          {turnViews.map(({ turn, idx, choiceAnswer, isChoiceAnswer, priorContents }) => (
            <TurnView
              key={turn.id}
              turn={turn}
              idx={idx}
              choiceAnswer={choiceAnswer}
              isChoiceAnswer={isChoiceAnswer}
              submittingChoice={submittingChoice}
              isCopied={copied === `turn-${turn.id}`}
              priorContents={priorContents}
              onChoice={handleChoice}
              onCopy={handleCopy}
              onDownload={handleDownload}
            />
          ))}

          {live && (() => {
            const liveDag = live.dag
            const liveTopText = live.text ?? ''
            const liveTopRuns = live.runs ?? []
            const liveDone = !streaming
            // Which text is the user-facing answer:
            //  - a DAG ran: the terminal node's answer IS the response (execute always
            //    delivers from the node now - there's no orchestrator-composed
            //    "synthesize" mode to prefer instead). liveTopText is the
            //    orchestrator's OWN narration (planning chatter, reasoning about the
            //    request) - never the answer when a DAG exists; falling back to it
            //    only masks a missing/incomplete terminal answer with unrelated text.
            //  - no DAG: the orchestrator answered directly, so its text IS the reply.
            const liveText = liveDag ? liveDagFinalText(liveDag) : liveTopText
            // The orchestrator's own activity (deciding to research, plan/execute calls).
            // get_user_choice is surfaced as its own QuestionBubble below, not a raw tool block.
            const orchActivity = visibleActivity(liveTopRuns.flatMap(r => r.activity))
            // A get_user_choice clarification awaiting an answer on the (paused) live turn.
            const choice = liveDone ? pendingChoice(liveTopRuns) : null
            // A paused node's mid-node HITL question (only possible once the run has
            // ended - the plan pauses the whole turn, so liveDone is implied).
            const nodeQuestion = liveDag ? pendingNodeQuestion(liveDag) : undefined
            // Show spinner while streaming until something VISIBLE arrives (DAG,
            // answer text, or visible activity). Keyed on orchActivity, not run
            // count - the orchestrator's top-level run is created empty on the
            // first event, so a run-count check blanks the dots before the plan.
            const showSpinner = showLiveSpinner({
              streaming,
              hasDag: !!liveDag,
              answerText: liveTopText,
              visibleActivityCount: orchActivity.length,
            })
            // Answer-bubble attribution: a DAG turn credits its terminal node (agent +
            // that node's own model/tokens); a plain reply credits the orchestrator,
            // whose own top-level run carries its model/usage once complete (item 1).
            const orchRun = liveTopRuns.find(r => r.runId === 'orchestrator')
            const answerAttribution = liveDag
              ? dagAnswerAttribution(liveDag)
              : { agent: 'orchestrator', model: orchRun?.model, tokens: orchRun?.totalTokens }
            // Skip the answer bubble when there's nothing in it yet.
            const hasAnswerBubble = showSpinner || (liveDag ? !!liveText : (orchActivity.length > 0 || !!liveTopText))
            const isChoiceAnswer = liveIsChoiceAnswer
            const copyKey = `live-${live.userText.slice(0, 20)}`
            return (
              // role="log" + aria-live: screen readers announce streamed tokens as they
              // arrive (aria-atomic=false → only the new text, not the whole region).
              <div key="live" role="log" aria-live="polite" aria-atomic="false">
                {/* User message - hidden when it's a clarification answer, or when the
                    turn has no user text/attachments at all (#434): a label/webhook-
                    triggered plan turn has no typed message, just its synthesized task
                    (rendered in the DAG bubble below), so there's nothing for this
                    bubble to show. */}
                {!isChoiceAnswer && (live.userText || liveAttachmentPreviews.length > 0) && (
                  <TriggerMessage
                    content={live.userText}
                    attachments={<AttachmentPreviews previews={liveAttachmentPreviews} />}
                    priorContents={livePriorContents}
                  />
                )}
                {/* Assistant response: DAG bubble → node question → answer bubble, as siblings */}
                <div className="flex justify-start">
                  <div className={liveDag ? 'w-full space-y-3' : 'w-auto space-y-3'}>
                    {liveDag && (
                      <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-2xl rounded-tl-sm px-5 py-4">
                        <DagBubbleHeader dag={liveDag} />
                        {/* The orchestrator agent wraps the DAG: show its own
                            activity (deciding to research, the plan/execute calls)
                            alongside the DAG. While running both are visible; once
                            done they collapse into "Steps". */}
                        {liveDone ? (
                          <details className="rounded-lg border border-gray-200 dark:border-gray-700">
                            <summary className="cursor-pointer select-none px-3 py-2 text-xs text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300">
                              Steps
                            </summary>
                            <div className="p-2 space-y-3">
                              {orchActivity.length > 0 && <ActivityList activity={orchActivity} />}
                              <DagView dag={liveDag} onRetryNode={handleRetryNode} />
                            </div>
                          </details>
                        ) : (
                          <div className="space-y-3">
                            {orchActivity.length > 0 && <LiveStatusLine activity={orchActivity} />}
                            <DagView
                              dag={liveDag}
                              onCancelNode={handleCancelNode}
                              onPauseNode={handlePauseNode}
                              onResumeNode={handleResumeNode}
                              onQueueNodeMessage={handleQueueNodeMessage}
                              onEditQueuedMessage={handleEditQueuedMessage}
                              onRemoveQueuedMessage={handleRemoveQueuedMessage}
                              onEditNodeTask={handleEditNodeTask}
                              onAnswerNodeQuestion={handleAnswerNode}
                            />
                          </div>
                        )}
                      </div>
                    )}
                    {nodeQuestion && (
                      <QuestionBubble
                        agent={nodeQuestion.agent}
                        question={nodeQuestion.question}
                        disabled={submittingChoice}
                        onSelect={answer => handleAnswerNode(nodeQuestion.nodeId, answer)}
                      />
                    )}
                    {hasAnswerBubble && (
                    <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-2xl rounded-tl-sm px-5 py-4">
                      {showSpinner ? (
                        <Dots className="h-5" size="w-2 h-2" />
                      ) : liveDag ? (
                        liveText && (
                          <>
                            <BubbleHeader agent={answerAttribution?.agent ?? 'orchestrator'} model={answerAttribution?.model} tokens={answerAttribution?.tokens} />
                            <AssistantText text={liveText} />
                          </>
                        )
                      ) : (
                        // No DAG: orchestrator answered directly (conversational or
                        // tool-based research where DAG events don't reach the frontend).
                        <div>
                          <BubbleHeader
                            agent="orchestrator"
                            model={answerAttribution?.model}
                            tokens={answerAttribution?.tokens}
                            status={streaming ? 'running' : 'done'}
                          />
                          {orchActivity.length > 0 && (
                            streaming ? <LiveStatusLine activity={orchActivity} /> : <ActivityList activity={orchActivity} />
                          )}
                          {/* Running is conveyed by the header's pulsing StatusDot
                              (#416) - no separate spinner dot while text streams in. */}
                          {liveTopText && <AssistantText text={liveTopText} />}
                        </div>
                      )}
                    </div>
                    )}
                    {choice && (
                      <QuestionBubble
                        agent="orchestrator"
                        question={choice.question}
                        options={choice.options}
                        disabled={submittingChoice}
                        onSelect={handleChoice}
                      />
                    )}
                    {liveText && (!streaming) && (
                      <div className="flex items-center gap-3 mt-1.5 px-1">
                        <button
                          onClick={() => handleCopy(copyKey, liveText)}
                          className="text-xs text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 transition-colors"
                        >
                          {copied === copyKey ? 'Copied!' : 'Copy'}
                        </button>
                        <button
                          onClick={() => handleDownload(liveText, state.turns.length)}
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
          })()}

          {/* Pending indicator: shown the instant a follow-up is submitted, while the
              previous turn is archived (the old `live` above still renders it, so it
              doesn't blink out). Replaced by the live turn once streaming starts. */}
          {state.submitting && state.pendingUserText != null && (
            <div>
              <div className="flex justify-end mb-3">
                <div className="max-w-2xl ml-auto">
                  <div className="bg-blue-600 text-white rounded-2xl rounded-tr-sm px-4 py-3 text-sm whitespace-pre-wrap">
                    {state.pendingUserText}
                  </div>
                </div>
              </div>
              <div className="flex justify-start">
                <div className="w-auto">
                  <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-2xl rounded-tl-sm px-5 py-4" role="status" aria-label="Thinking">
                    <Dots className="h-5" size="w-2 h-2" />
                  </div>
                </div>
              </div>
            </div>
          )}

          {error && (
            <div className="rounded-md bg-red-50 dark:bg-red-950/30 border border-red-200 dark:border-red-800 px-4 py-3 text-sm text-red-700 dark:text-red-400">
              {error}
            </div>
          )}
        </div>

        <Composer
          disabled={!activeChatId}
          streaming={streaming}
          onSubmit={submitMessage}
          onStop={handleStop}
          queue={state.queue}
          onRemoveQueued={handleRemoveQueued}
        />
      </div>
    </div>
  )
}
