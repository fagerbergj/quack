import { useState, useEffect, useLayoutEffect, useRef, useCallback, useMemo } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { api, type ChatSummary } from '../api'
import { AssistantText, ActivityList, Dots } from '../components/AgentParts'
import { ChoicePrompt } from '../components/ChoicePrompt'
import { DagView } from '../components/DagView'
import { Composer } from '../components/Composer'
import { ChatList } from '../components/ChatList'
import { TurnView, visibleActivity } from '../components/TurnView'
import { useChatStore, useChatState } from '../state/ChatStoreProvider'
import { activityFromTurn, isTurnInProgress, type DagTurnState } from '../state/chatStore'
import { pendingChoice, showLiveSpinner, type AgentRun } from '../components/messageParts'
import { AttachmentPreviews } from '../components/AttachmentUI'

// liveDagFinalText extracts the answer from the terminal node's accumulated answer.
// Used as a fallback when the orchestrator presents no top-level text of its own —
// the answer then lives in the last DAG node's nodeAnswer.
function liveDagFinalText(dag: DagTurnState): string {
  if (!dag.nodes.length) return ''
  const hasSuccessor = new Set<string>()
  for (const n of dag.nodes) for (const dep of n.depends_on ?? []) hasSuccessor.add(dep)
  const finalNode = dag.nodes.find(n => !hasSuccessor.has(n.id))
  if (!finalNode) return ''
  return dag.nodeAnswer[finalNode.id] ?? ''
}

// executeDeliverMode reports whether the orchestrator's most recent execute call
// used end_turn=true (deliver). In deliver mode the terminal node's answer IS the
// user-facing answer; otherwise the orchestrator composes it (synthesize) and its
// own text is the answer. Read off the execute tool call(s) in the orchestrator's
// own (top-level) runs — the LAST call wins, since the model may call execute
// twice (e.g. once to read the result, then again with end_turn=true).
function executeDeliverMode(topRuns: AgentRun[]): boolean {
  let deliver = false
  for (const run of topRuns) {
    for (const a of run.activity) {
      if (a.kind === 'tool' && a.tool.name === 'execute') {
        deliver = a.tool.args?.end_turn === true
      }
    }
  }
  return deliver
}

export default function Chat() {
  const { chatId: urlChatId } = useParams<{ chatId?: string }>()
  const navigate = useNavigate()

  const store = useChatStore()
  const [chats, setChats] = useState<ChatSummary[]>([])
  const [activeChatId, setActiveChatId] = useState<string | null>(null)
  const state = useChatState(activeChatId)
  const streaming = state.live?.streaming ?? false
  const error = state.error
  const live = state.live
  const [systemPrompt, setSystemPrompt] = useState('')
  const [showSettings, setShowSettings] = useState(false)
  const [chatListOpen, setChatListOpen] = useState(false)
  const [copied, setCopied] = useState<string | null>(null)
  const [submittingChoice, setSubmittingChoice] = useState(false)
  const [liveAttachmentPreviews, setLiveAttachmentPreviews] = useState<{url: string; mime: string; name: string}[]>([])
  const scrollRef = useRef<HTMLDivElement>(null)

  // Open a chat scrolled to the latest message (and snap down as turns complete),
  // not pinned to the top of a long history. Keyed on turn count so it fires after
  // the async seed lands. Doesn't follow mid-stream tokens (not requested).
  useLayoutEffect(() => {
    const el = scrollRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [activeChatId, state.turns.length])

  useEffect(() => {
    const stored = localStorage.getItem('theme')
    if (stored === 'dark' || (!stored && window.matchMedia('(prefers-color-scheme: dark)').matches)) {
      document.documentElement.classList.add('dark')
    }
  }, [])

  const loadChats = useCallback(async () => {
    const result = await api.listChats()
    setChats(result.data)
    return result.data
  }, [])

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
      // Reconnect to a run still in progress (e.g. this browser after a refresh):
      // the POST body stream is gone, so subscribe to the hub. attach no-ops if
      // this client already streams (it posted the run) — no double-subscribe.
      if (isTurnInProgress(detail.turns[detail.turns.length - 1])) {
        store.attach(activeChatId)
      }
    }).catch(() => {})
    return () => {
      cancelled = true
      store.detachStream(activeChatId)
    }
  }, [activeChatId])

  function activateChat(id: string) {
    setActiveChatId(id)
    navigate(`/chat/${id}`)
  }

  async function handleNewChat() {
    const chat = await api.createChat({ system_prompt: systemPrompt.trim() || undefined })
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

  const handleStopNode = useCallback((nodeId: string) => {
    if (activeChatId) store.cancelNode(activeChatId, nodeId)
  }, [activeChatId, store])

  const handleSteerNode = useCallback((nodeId: string, guidance: string) => {
    if (activeChatId) store.steerNode(activeChatId, nodeId, guidance)
  }, [activeChatId, store])

  const handleRetryNode = useCallback((nodeId: string, guidance?: string) => {
    if (activeChatId) store.retryNode(activeChatId, nodeId, guidance)
  }, [activeChatId, store])

  // handleAnswerNode answers a paused node's question (mid-node HITL) by sending
  // the answer as the next message — the backend delivers it to the paused node.
  const handleAnswerNode = useCallback((_nodeId: string, answer: string) => {
    if (!activeChatId) return
    void store.submit(activeChatId, answer, undefined, title => {
      setChats(prev => prev.map(c => c.id === activeChatId ? { ...c, title } : c))
    }).then(() => loadChats().then(data => setChats(data)))
  }, [activeChatId, store, loadChats])

  const submitMessage = useCallback((text: string, files: File[], previews: { url: string; mime: string; name: string }[]) => {
    if (!activeChatId) return
    setLiveAttachmentPreviews(previews)
    store.submit(activeChatId, text, files.length > 0 ? files : undefined, title => {
      setChats(prev => prev.map(c => c.id === activeChatId ? { ...c, title } : c))
    }).then(() => loadChats().then(data => setChats(data)))
  }, [activeChatId, store, loadChats])

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
  // only `live` changes) — the key to not re-parsing every turn's markdown mid-stream.
  const liveUserText = live?.userText
  const turnViews = useMemo(() => state.turns.map((turn, idx, arr) => {
    const turnChoice = pendingChoice(activityFromTurn(turn))
    // The answer to a clarification is the next turn's input, or — for the last
    // turn — the live turn's input. Undefined means it's still answerable.
    const next = arr[idx + 1]
    const choiceAnswer = turnChoice ? (next ? next.input.content : liveUserText) : undefined
    // This turn's input is itself the answer to the previous turn's clarification.
    const prev = arr[idx - 1]
    const isChoiceAnswer = prev ? pendingChoice(activityFromTurn(prev)) != null : false
    return { turn, idx, choiceAnswer, isChoiceAnswer }
  }), [state.turns, liveUserText])

  // The live turn is a clarification answer when the last completed turn asked one.
  const lastTurn = state.turns[state.turns.length - 1]
  const liveIsChoiceAnswer = lastTurn ? pendingChoice(activityFromTurn(lastTurn)) != null : false

  return (
    <div className="flex h-screen bg-gray-50 dark:bg-gray-900 text-gray-900 dark:text-white">
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
            <h1 className="text-base font-semibold text-gray-900 dark:text-white truncate">
              {chats.find(s => s.id === activeChatId)?.title || (activeChatId ? 'New chat' : 'Chat')}
            </h1>
          </div>
          <button
            onClick={() => setShowSettings(s => !s)}
            className={`text-xs px-3 py-1.5 rounded border transition-colors flex-shrink-0 ${showSettings ? 'bg-gray-100 dark:bg-gray-700 border-gray-300 dark:border-gray-600 text-gray-700 dark:text-gray-200' : 'border-gray-200 dark:border-gray-600 text-gray-500 dark:text-gray-400 hover:text-gray-700 dark:hover:text-gray-200 hover:border-gray-300 dark:hover:border-gray-500'}`}
          >
            Settings
          </button>
        </div>

        {showSettings && (
          <div className="px-6 py-4 border-b border-gray-200 dark:border-gray-700 bg-gray-50 dark:bg-gray-900">
            <div className="flex items-start gap-6">
              <div className="flex-1">
                <label className="text-xs font-medium text-gray-600 dark:text-gray-300 mb-1 block">
                  System prompt (applied to new chats)
                </label>
                <textarea
                  className="w-full rounded border border-gray-300 dark:border-gray-600 px-2 py-1.5 text-xs focus:outline-none focus:ring-2 focus:ring-blue-500 resize-none dark:bg-gray-700 dark:text-gray-100 dark:placeholder-gray-400"
                  rows={3}
                  placeholder="Add context to guide answers…"
                  value={systemPrompt}
                  onChange={e => setSystemPrompt(e.target.value)}
                />
              </div>
            </div>
          </div>
        )}

        <div ref={scrollRef} className="flex-1 overflow-y-auto px-6 py-6 space-y-6">
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

          {turnViews.map(({ turn, idx, choiceAnswer, isChoiceAnswer }) => (
            <TurnView
              key={turn.id}
              turn={turn}
              idx={idx}
              choiceAnswer={choiceAnswer}
              isChoiceAnswer={isChoiceAnswer}
              submittingChoice={submittingChoice}
              isCopied={copied === `turn-${turn.id}`}
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
            const deliverMode = liveDag ? executeDeliverMode(liveTopRuns) : false
            // Which text is the user-facing answer:
            //  - deliver (execute end_turn=true): the terminal node's answer IS the
            //    response, streaming token-by-token via nodeAnswer.
            //  - synthesize: the orchestrator composes the final answer (its own
            //    top-level text); fall back to the terminal node's answer if it
            //    produced none — so a missed deliver-detection never blanks the reply.
            //  - no DAG: the orchestrator answered directly.
            const liveText = liveDag
              ? (deliverMode ? liveDagFinalText(liveDag) : (liveTopText || liveDagFinalText(liveDag)))
              : liveTopText
            // The orchestrator's own activity (deciding to research, plan/execute calls).
            // get_user_choice is surfaced as the ChoicePrompt below, not as a raw tool block.
            const orchActivity = visibleActivity(liveTopRuns.flatMap(r => r.activity))
            // A get_user_choice clarification awaiting an answer on the (paused) live turn.
            const choice = liveDone ? pendingChoice(liveTopRuns) : null
            // Show spinner while streaming until something VISIBLE arrives (DAG,
            // answer text, or visible activity). Keyed on orchActivity, not run
            // count — the orchestrator's top-level run is created empty on the
            // first event, so a run-count check blanks the dots before the plan.
            const showSpinner = showLiveSpinner({
              streaming,
              hasDag: !!liveDag,
              answerText: liveTopText,
              visibleActivityCount: orchActivity.length,
            })
            // Skip the bubble when there's nothing in it (e.g. a pending clarification
            // at rest) — the ChoicePrompt renders on its own below.
            const hasLiveBubbleContent = showSpinner || !!liveDag || !!liveText || orchActivity.length > 0
            const isChoiceAnswer = liveIsChoiceAnswer
            const copyKey = `live-${live.userText.slice(0, 20)}`
            return (
              // role="log" + aria-live: screen readers announce streamed tokens as they
              // arrive (aria-atomic=false → only the new text, not the whole region).
              <div key="live" role="log" aria-live="polite" aria-atomic="false">
                {/* User message (hidden when it's a clarification answer) */}
                {!isChoiceAnswer && (
                  <div className="flex justify-end mb-3">
                    <div className="max-w-2xl ml-auto">
                      <div className="bg-blue-600 text-white rounded-2xl rounded-tr-sm px-4 py-3 text-sm whitespace-pre-wrap">
                        <AttachmentPreviews previews={liveAttachmentPreviews} />
                        {live.userText}
                      </div>
                    </div>
                  </div>
                )}
                {/* Assistant response */}
                <div className="flex justify-start">
                  <div className={liveDag ? 'w-full' : 'w-auto'}>
                    {hasLiveBubbleContent && (
                    <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-2xl rounded-tl-sm px-5 py-4">
                      {showSpinner ? (
                        <Dots className="h-5" size="w-2 h-2" />
                      ) : liveDag ? (
                        <>
                          {/* The orchestrator agent wraps the DAG: show its own
                              activity (deciding to research, the plan/execute calls)
                              alongside the DAG. While running both are visible; once
                              done they collapse into "Research steps" so only the
                              final answer remains. */}
                          {liveDone ? (
                            <details className="mb-4 rounded-lg border border-gray-200 dark:border-gray-700">
                              <summary className="cursor-pointer select-none px-3 py-2 text-xs text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300">
                                ▸ Research steps
                              </summary>
                              <div className="p-2 space-y-3">
                                {orchActivity.length > 0 && <ActivityList activity={orchActivity} />}
                                <DagView dag={liveDag} onRetryNode={handleRetryNode} onAnswerNode={handleAnswerNode} />
                              </div>
                            </details>
                          ) : (
                            <div className="space-y-3">
                              {orchActivity.length > 0 && <ActivityList activity={orchActivity} />}
                              <DagView dag={liveDag} onStopNode={handleStopNode} onSteerNode={handleSteerNode} />
                            </div>
                          )}
                          {liveText && (
                            <div className={liveDone ? '' : 'mt-4 pt-4 border-t border-gray-100 dark:border-gray-700'}>
                              <AssistantText text={liveText} />
                            </div>
                          )}
                        </>
                      ) : (
                        // No DAG: orchestrator answered directly (conversational or
                        // tool-based research where DAG events don't reach the frontend).
                        <div>
                          {orchActivity.length > 0 && (
                            <ActivityList activity={orchActivity} />
                          )}
                          {liveTopText
                            ? <AssistantText text={liveTopText} />
                            : streaming && <Dots />
                          }
                        </div>
                      )}
                    </div>
                    )}
                    {choice && (
                      <ChoicePrompt
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
        />
      </div>
    </div>
  )
}
