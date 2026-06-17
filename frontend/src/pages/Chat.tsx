import { useState, useEffect, useCallback, useRef } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { api, type ChatSummary } from '../api'
import { AssistantText, ActivityList, Dots } from '../components/AgentParts'
import { DagView } from '../components/DagView'
import { useChatStore, useChatState } from '../state/ChatStoreProvider'
import { dagFromTurn, textFromTurn, type DagTurnState } from '../state/chatStore'
import type { Turn, DagOutputItem } from '../generated'

import { AttachmentPreviews, AttachmentStrip, type AttachmentItem } from '../components/AttachmentUI'

function relativeDate(iso: string): string {
  const diff = Date.now() - new Date(iso).getTime()
  const mins = Math.floor(diff / 60000)
  if (mins < 1) return 'just now'
  if (mins < 60) return `${mins}m ago`
  const hrs = Math.floor(mins / 60)
  if (hrs < 24) return `${hrs}h ago`
  const days = Math.floor(hrs / 24)
  if (days < 7) return `${days}d ago`
  return new Date(iso).toLocaleDateString()
}

// dagTurnStateFromItem converts a persisted DagOutputItem into a DagTurnState
// suitable for DagView. Runs/answers are empty (streaming content isn't persisted).
function dagTurnStateFromItem(item: DagOutputItem): DagTurnState {
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

export default function Chat({ systemPrompt: globalSystemPrompt }: { systemPrompt: string }) {
  const { chatId: urlChatId } = useParams<{ chatId?: string }>()
  const navigate = useNavigate()

  const store = useChatStore()
  const [chats, setChats] = useState<ChatSummary[]>([])
  const [activeChatId, setActiveChatId] = useState<string | null>(null)
  const state = useChatState(activeChatId)
  const streaming = state.live?.streaming ?? false
  const error = state.error
  const live = state.live
  const [input, setInput] = useState('')
  const [systemPrompt, setSystemPrompt] = useState(globalSystemPrompt)
  const [showSettings, setShowSettings] = useState(false)
  const [chatListOpen, setChatListOpen] = useState(false)
  const [copied, setCopied] = useState<string | null>(null)
  const [attachments, setAttachments] = useState<AttachmentItem[]>([])
  const [liveAttachmentPreviews, setLiveAttachmentPreviews] = useState<{url: string; mime: string; name: string}[]>([])
  const fileInputRef = useRef<HTMLInputElement>(null)

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
    }).catch(() => {})
    return () => { cancelled = true }
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

  function handleCopy(key: string, content: string) {
    navigator.clipboard.writeText(content)
    setCopied(key)
    setTimeout(() => setCopied(null), 2000)
  }

  function handleDownload(content: string, idx: number) {
    const h1 = content.match(/^#\s+(.+)$/m)?.[1]?.trim()
    const slug = h1 ? h1.replace(/[^\w\s-]/g, '').trim().replace(/\s+/g, '-') : `answer-${idx + 1}`
    const blob = new Blob([content], { type: 'text/markdown' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `${slug}.md`
    a.click()
    URL.revokeObjectURL(url)
  }

  function handleStop() {
    if (activeChatId) store.stop(activeChatId)
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    const trimmed = input.trim()
    if (!trimmed || streaming) return
    if (!activeChatId) return
    setInput('')
    const items = attachments.slice()
    setAttachments([])
    setLiveAttachmentPreviews(items.map(a => ({ url: a.url, mime: a.file.type, name: a.file.name })))
    const files = items.map(a => a.file)
    await store.submit(activeChatId, trimmed, files.length > 0 ? files : undefined, title => {
      setChats(prev => prev.map(c => c.id === activeChatId ? { ...c, title } : c))
    })
    await loadChats().then(data => setChats(data))
  }

  function handleKeyDown(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      handleSubmit(e as unknown as React.FormEvent)
    }
  }

  // Build the display list: completed turns + the live turn if active.
  type DisplayItem =
    | { kind: 'turn'; turn: Turn; idx: number }
    | { kind: 'live' }

  const displayItems: DisplayItem[] = state.turns.map((turn, idx) => ({ kind: 'turn', turn, idx }))
  if (live) displayItems.push({ kind: 'live' })

  return (
    <div className="flex h-screen bg-gray-50 dark:bg-gray-900 text-gray-900 dark:text-white">
      {chatListOpen && (
        <div
          className="md:hidden fixed inset-0 z-30 bg-black/50"
          onClick={() => setChatListOpen(false)}
          aria-hidden="true"
        />
      )}

      <div className={`
        fixed md:static inset-y-0 left-0 z-40
        w-[250px] flex-shrink-0 flex flex-col
        border-r border-gray-200 dark:border-gray-700
        bg-white dark:bg-gray-800
        transition-transform duration-200
        md:translate-x-0
        ${chatListOpen ? 'translate-x-0' : '-translate-x-full md:translate-x-0'}
      `}>
        <div className="p-3 border-b border-gray-200 dark:border-gray-700 flex items-center gap-2">
          <button
            onClick={handleNewChat}
            className="flex-1 text-sm px-3 py-2 rounded-lg bg-blue-600 text-white hover:bg-blue-700 transition-colors font-medium"
          >
            New Chat
          </button>
          <button
            onClick={() => setChatListOpen(false)}
            className="md:hidden text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 p-1.5 rounded transition-colors"
            aria-label="Close chat list"
          >
            ✕
          </button>
        </div>
        <div className="flex-1 overflow-y-auto">
          {chats.length === 0 && (
            <div className="text-xs text-gray-400 dark:text-gray-500 text-center py-6 px-3">No conversations yet</div>
          )}
          {chats.map(s => (
            <div
              key={s.id}
              onClick={() => { activateChat(s.id); setChatListOpen(false) }}
              className={`group relative flex flex-col px-3 py-2.5 cursor-pointer border-b border-gray-100 dark:border-gray-700 hover:bg-gray-50 dark:hover:bg-gray-700 transition-colors ${activeChatId === s.id ? 'bg-blue-50 dark:bg-blue-900/30' : ''}`}
            >
              <span className={`text-sm truncate pr-6 ${activeChatId === s.id ? 'text-blue-700 dark:text-blue-400 font-medium' : 'text-gray-800 dark:text-gray-100'}`}>
                {s.title || 'New chat'}
              </span>
              <span className="text-xs text-gray-400 dark:text-gray-500 mt-0.5">{relativeDate(s.updated_at)}</span>
              <button
                onClick={e => handleDeleteChat(s.id, e)}
                className="absolute right-2 top-1/2 -translate-y-1/2 opacity-0 group-hover:opacity-100 text-gray-400 hover:text-red-500 transition-opacity p-1 rounded"
              >
                ×
              </button>
            </div>
          ))}
        </div>
      </div>

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

        <div className="flex-1 overflow-y-auto px-6 py-6 space-y-6">
          {!activeChatId && (
            <div className="text-center text-gray-400 dark:text-gray-500 text-sm mt-20">
              Select or start a chat
            </div>
          )}
          {activeChatId && displayItems.length === 0 && !streaming && (
            <div className="text-center text-gray-400 dark:text-gray-500 text-sm mt-20">
              Ask a question
            </div>
          )}

          {displayItems.map((item, idx) => {
            if (item.kind === 'turn') {
              const { turn } = item
              const dagItem = dagFromTurn(turn)
              const dagState = dagItem ? dagTurnStateFromItem(dagItem) : undefined
              const text = textFromTurn(turn)
              const copyKey = `turn-${turn.id}`
              return (
                <div key={turn.id}>
                  {/* User message */}
                  <div className="flex justify-end mb-3">
                    <div className="max-w-2xl ml-auto">
                      <div className="bg-blue-600 text-white rounded-2xl rounded-tr-sm px-4 py-3 text-sm whitespace-pre-wrap">
                        {turn.input.content}
                      </div>
                    </div>
                  </div>
                  {/* Assistant response */}
                  <div className="flex justify-start">
                    <div className="w-full">
                      <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-2xl rounded-tl-sm px-5 py-4">
                        {dagState ? (
                          <>
                            <details className="mb-4 rounded-lg border border-gray-200 dark:border-gray-700">
                              <summary className="cursor-pointer select-none px-3 py-2 text-xs text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300">
                                ▸ Research steps
                              </summary>
                              <div className="p-2">
                                <DagView dag={dagState} />
                              </div>
                            </details>
                            {text && <AssistantText text={text} />}
                          </>
                        ) : (
                          <AssistantText text={text} />
                        )}
                      </div>
                      {text && (
                        <div className="flex items-center gap-3 mt-1.5 px-1">
                          <button
                            onClick={() => handleCopy(copyKey, text)}
                            className="text-xs text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 transition-colors"
                          >
                            {copied === copyKey ? 'Copied!' : 'Copy'}
                          </button>
                          <button
                            onClick={() => handleDownload(text, idx)}
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
            }

            // Live turn
            const liveDag = live!.dag
            const liveTopText = live!.text ?? ''
            const liveTopRuns = live!.runs ?? []
            const liveDone = !streaming
            // When a DAG is present (streaming or done) the terminal node's answer
            // is the response — it streams token-by-token via nodeAnswer. Ignore any
            // top-level orchestrator text (pre-plan preamble). Only use liveTopText
            // when no DAG exists (orchestrator answered directly).
            const liveText = liveDag ? liveDagFinalText(liveDag) : liveTopText
            // Show spinner only when streaming and there's nothing to show yet.
            const showSpinner = streaming && !liveDag && !liveTopText && liveTopRuns.length === 0
            const copyKey = `live-${live!.userText.slice(0, 20)}`
            return (
              <div key="live">
                {/* User message */}
                <div className="flex justify-end mb-3">
                  <div className="max-w-2xl ml-auto">
                    <div className="bg-blue-600 text-white rounded-2xl rounded-tr-sm px-4 py-3 text-sm whitespace-pre-wrap">
                      <AttachmentPreviews previews={liveAttachmentPreviews} />
                      {live!.userText}
                    </div>
                  </div>
                </div>
                {/* Assistant response */}
                <div className="flex justify-start">
                  <div className={liveDag ? 'w-full' : 'w-auto'}>
                    <div className="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-700 rounded-2xl rounded-tl-sm px-5 py-4">
                      {showSpinner ? (
                        <span className="flex items-center gap-1 h-5">
                          <span className="w-2 h-2 rounded-full bg-gray-400 animate-bounce [animation-delay:-0.3s]" />
                          <span className="w-2 h-2 rounded-full bg-gray-400 animate-bounce [animation-delay:-0.15s]" />
                          <span className="w-2 h-2 rounded-full bg-gray-400 animate-bounce" />
                        </span>
                      ) : liveDag && !liveDone ? (
                        <>
                          <DagView dag={liveDag} />
                          {liveText && (
                            <div className="mt-4 pt-4 border-t border-gray-100 dark:border-gray-700">
                              <AssistantText text={liveText} />
                            </div>
                          )}
                        </>
                      ) : liveDag && liveDone ? (
                        <>
                          <details className="mb-4 rounded-lg border border-gray-200 dark:border-gray-700">
                            <summary className="cursor-pointer select-none px-3 py-2 text-xs text-gray-500 dark:text-gray-400 hover:text-gray-600 dark:hover:text-gray-300">
                              ▸ Research steps
                            </summary>
                            <div className="p-2">
                              <DagView dag={liveDag} />
                            </div>
                          </details>
                          {liveText && <AssistantText text={liveText} />}
                        </>
                      ) : (
                        // No DAG: orchestrator answered directly (conversational or
                        // tool-based research where DAG events don't reach the frontend).
                        <div>
                          {liveTopRuns.length > 0 && (
                            <ActivityList activity={liveTopRuns.flatMap(r => r.activity)} />
                          )}
                          {liveTopText
                            ? <AssistantText text={liveTopText} />
                            : streaming && <Dots />
                          }
                        </div>
                      )}
                    </div>
                    {liveText && (!streaming) && (
                      <div className="flex items-center gap-3 mt-1.5 px-1">
                        <button
                          onClick={() => handleCopy(copyKey, liveText)}
                          className="text-xs text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 transition-colors"
                        >
                          {copied === copyKey ? 'Copied!' : 'Copy'}
                        </button>
                        <button
                          onClick={() => handleDownload(liveText, displayItems.length)}
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
          })}

          {error && (
            <div className="rounded-md bg-red-50 dark:bg-red-950/30 border border-red-200 dark:border-red-800 px-4 py-3 text-sm text-red-700 dark:text-red-400">
              {error}
            </div>
          )}
        </div>

        <div className="border-t border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 px-6 py-4">
          <form onSubmit={handleSubmit} className="flex flex-col gap-2">
            <AttachmentStrip
              attachments={attachments}
              onRemove={i => setAttachments(prev => {
                if (prev[i].url) URL.revokeObjectURL(prev[i].url)
                return prev.filter((_, j) => j !== i)
              })}
            />
            <div className="flex gap-2 items-end">
              <input
                ref={fileInputRef}
                type="file"
                accept="image/*,audio/*"
                multiple
                className="hidden"
                onChange={e => {
                  if (e.target.files) {
                    const items: AttachmentItem[] = Array.from(e.target.files).map(f => ({
                      file: f,
                      url: URL.createObjectURL(f),
                    }))
                    setAttachments(prev => [...prev, ...items])
                  }
                  e.target.value = ''
                }}
              />
              <button
                type="button"
                onClick={() => fileInputRef.current?.click()}
                disabled={streaming || !activeChatId}
                className="p-3 rounded-xl border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-500 dark:text-gray-400 hover:bg-gray-50 dark:hover:bg-gray-600 disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
                aria-label="Attach file"
                title="Attach image or audio"
              >
                📎
              </button>
              <textarea
                className="flex-1 rounded-xl border border-gray-300 dark:border-gray-600 px-4 py-3 text-sm focus:outline-none focus:ring-2 focus:ring-blue-500 resize-none disabled:opacity-50 dark:bg-gray-700 dark:text-gray-100 dark:placeholder-gray-400"
                rows={1}
                placeholder={activeChatId ? 'Ask something… (Enter to send, Shift+Enter for newline)' : 'Select or start a chat first'}
                value={input}
                onChange={e => setInput(e.target.value)}
                onKeyDown={handleKeyDown}
                disabled={streaming || !activeChatId}
              />
              {streaming ? (
                <button
                  type="button"
                  onClick={handleStop}
                  className="px-4 py-3 rounded-xl bg-gray-200 dark:bg-gray-700 text-gray-700 dark:text-gray-200 text-sm font-medium hover:bg-gray-300 dark:hover:bg-gray-600 transition-colors whitespace-nowrap"
                >
                  Stop
                </button>
              ) : (
                <button
                  type="submit"
                  disabled={(!input.trim() && attachments.length === 0) || !activeChatId}
                  className="px-4 py-3 rounded-xl bg-blue-600 text-white text-sm font-medium hover:bg-blue-700 disabled:opacity-50 disabled:cursor-not-allowed transition-colors whitespace-nowrap"
                >
                  Send
                </button>
              )}
            </div>
          </form>
        </div>
      </div>
    </div>
  )
}
