import type { Meta, StoryObj } from '@storybook/react-vite'
import { useEffect } from 'react'
import Chat from './Chat'
import { ChatStoreProvider, useChatStore } from '../state/ChatStoreProvider'
import type { ChatSummary, ChatDetail, Turn } from '../generated'

// Chat talks to the real REST client (api.ts) and a ChatStoreProvider context -
// same stub-global.fetch pattern as ArtifactPanel/Memory (no MSW in this repo),
// routed on the chat id in the URL. useChatId() reads window.location, so each
// story also pushes its own /chat/:id path before mounting. attach() derives
// the "live" turn straight from the last persisted turn (no SSE events needed
// for a static fixture) but still opens a real EventSource - stubbed here to a
// no-op so a story never fires an actual network request.
class FakeEventSource extends EventTarget {
  close() {}
}
window.EventSource = FakeEventSource as unknown as typeof EventSource

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
}

const now = new Date('2026-09-01T12:00:00Z').toISOString()

function summary(over: Partial<ChatSummary> & { id: string }): ChatSummary {
  return { title: 'Untitled chat', system_prompt: '', created_at: now, updated_at: now, status: 'idle', ...over }
}

function userTurn(id: string, content: string, answer: string): Turn {
  return {
    id,
    created_at: now,
    input: { role: 'user', content },
    output: [{ id: `${id}-out`, type: 'message', status: 'completed', content: [{ type: 'output_text', text: answer }] }],
    model: 'qwen3-30b-a3b',
  }
}

function dagTurn(id: string, content: string): Turn {
  return {
    id,
    created_at: now,
    input: { role: 'user', content },
    output: [{
      id: `${id}-dag`,
      type: 'quack:dag',
      status: 'in_progress',
      plan_id: 'plan-1',
      nodes: [{ id: 'r1', agent: 'web-researcher', task: 'Research the topic.', depends_on: [] }],
      edges: [],
      node_states: { r1: { status: 'running', started_at_ms: 0 } },
    }],
  }
}

function finishedDagTurn(id: string, content: string): Turn {
  return {
    id,
    created_at: now,
    input: { role: 'user', content },
    output: [{
      id: `${id}-dag`,
      type: 'quack:dag',
      status: 'completed',
      plan_id: 'plan-1',
      nodes: [
        { id: 'r1', agent: 'web-researcher', task: 'Research Dublin travel tips.', depends_on: [], artifact: 'document:travel-guide' },
        { id: 'synth', agent: 'synthesizer', task: 'Write up the guide.', depends_on: ['r1'] },
      ],
      edges: [{ from: 'r1', to: 'synth' }],
      node_states: {
        r1: { status: 'done', started_at_ms: 0, finished_at_ms: 12_000, total_tokens: 1_200, model: 'qwen3-30b-a3b' },
        synth: { status: 'done', started_at_ms: 12_000, finished_at_ms: 20_000, total_tokens: 400, model: 'qwen3-30b-a3b' },
      },
    }],
  }
}

// stubChat routes global.fetch on the chat id in the URL, like ArtifactPanel's
// story. Called from each story's decorator so a fresh ChatStoreProvider and
// fetch stub pair with the pathname pushed for that story's chatId.
function stubChat(chatId: string, chats: ChatSummary[], detail?: ChatDetail, artifacts: unknown[] = []) {
  window.fetch = async (input: RequestInfo | URL) => {
    const url = decodeURIComponent(input instanceof Request ? input.url : String(input))
    if (url.includes('/artifacts')) return jsonResponse({ data: artifacts })
    if (detail && url.includes(`/chats/${chatId}`) && !url.includes('/chats?')) return jsonResponse(detail)
    return jsonResponse({ data: chats })
  }
}

function withChat(chatId: string | undefined, chats: ChatSummary[], detail?: ChatDetail, artifacts: unknown[] = []) {
  return [(Story: React.ComponentType) => {
    window.history.replaceState(null, '', chatId ? `/chat/${chatId}` : '/chat')
    stubChat(chatId ?? '', chats, detail, artifacts)
    return <ChatStoreProvider><div className="h-[37.5rem]"><Story /></div></ChatStoreProvider>
  }]
}

const meta: Meta<typeof Chat> = {
  title: 'Pages/Chat',
  component: Chat,
  parameters: { layout: 'fullscreen' },
}
export default meta

type Story = StoryObj<typeof Chat>
const baseArgs = { navOpen: false, onToggleNav: () => {} }

// No chats at all - the "Select or start a chat" empty state, sidebar empty too.
export const EmptyChat: Story = {
  args: baseArgs,
  decorators: withChat(undefined, []),
}

// A running DAG node, attached straight from the persisted turn (#463) - no
// SSE traffic needed for the fixture to render mid-stream chrome.
export const StreamingTurn: Story = {
  args: baseArgs,
  decorators: withChat(
    'chat-streaming',
    [summary({ id: 'chat-streaming', title: 'Dublin trip planning', status: 'running' })],
    { ...summary({ id: 'chat-streaming', title: 'Dublin trip planning', status: 'running' }), turns: [dagTurn('t1', 'Plan a trip to Dublin')], usage: {} },
  ),
}

// A completed turn with a finished DAG (one node carrying its own output artifact).
export const FinishedTurnWithArtifacts: Story = {
  args: baseArgs,
  decorators: withChat(
    'chat-done',
    [summary({ id: 'chat-done', title: 'Dublin travel guide', status: 'idle' })],
    { ...summary({ id: 'chat-done', title: 'Dublin travel guide', status: 'idle' }), turns: [finishedDagTurn('t1', 'Write a Dublin travel guide')], usage: {} },
    [{ name: 'document:travel-guide', kind: 'document', class: 'structured', latest_revision: 1, lineage: { node_id: 'r1', author: 'worker' }, revisions: [] }],
  ),
}

// Archived chats are read-only: the header badge shows it, the title can't be
// renamed, and the composer is disabled (see Chat.tsx's `isArchived` wiring).
export const ArchivedChat: Story = {
  args: baseArgs,
  decorators: withChat(
    'chat-archived',
    [],
    { ...summary({ id: 'chat-archived', title: 'Old research thread', status: 'idle', archived: true }), turns: [userTurn('t1', 'What is the capital of Ireland?', 'Dublin.')], usage: {} },
  ),
}

// A finished chat with follow-ups queued behind it (Composer's queue chips) -
// queueTurn is the store's own client-side API (no endpoint involved), so the
// decorator calls it once after the store mounts rather than reaching past
// Chat's props into ChatStoreProvider internals.
export const QueuedFollowUps: Story = {
  args: baseArgs,
  decorators: [
    (Story => {
      window.history.replaceState(null, '', '/chat/chat-queued')
      const chat = summary({ id: 'chat-queued', title: 'Trip planning', status: 'idle' })
      stubChat('chat-queued', [chat], { ...chat, turns: [userTurn('t1', 'Where should I stay?', 'Try Temple Bar or the Docklands.')], usage: {} })
      function Seed() {
        const store = useChatStore()
        useEffect(() => {
          store.queueTurn('chat-queued', 'What about restaurants?')
          store.queueTurn('chat-queued', 'And a day trip to Howth?')
        }, [store])
        return <Story />
      }
      return <ChatStoreProvider><div className="h-[37.5rem]"><Seed /></div></ChatStoreProvider>
    }),
  ],
}

export const MobileViewport390: Story = {
  ...FinishedTurnWithArtifacts,
  parameters: { layout: 'fullscreen', renderCheck: { viewports: ['mobile'] } },
  decorators: [
    (Story => {
      window.history.replaceState(null, '', '/chat/chat-done')
      const chat = summary({ id: 'chat-done', title: 'Dublin travel guide', status: 'idle' })
      stubChat('chat-done', [chat], { ...chat, turns: [finishedDagTurn('t1', 'Write a Dublin travel guide')], usage: {} }, [
        { name: 'document:travel-guide', kind: 'document', class: 'structured', latest_revision: 1, lineage: { node_id: 'r1', author: 'worker' }, revisions: [] },
      ])
      return (
        <ChatStoreProvider>
          <div className="w-[390px] h-[844px] overflow-hidden border border-gray-300 dark:border-gray-600">
            <Story />
          </div>
        </ChatStoreProvider>
      )
    }),
  ],
}
