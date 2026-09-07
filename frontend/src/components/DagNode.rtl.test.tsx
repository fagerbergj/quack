// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { cleanup, render as rtlRender, screen } from '@testing-library/react'
import type { ReactElement } from 'react'
import userEvent from '@testing-library/user-event'
import { DagNode } from './DagNode'
import type { DagNodeDef } from '../state/agentStream'
import { client } from '../generated/client.gen'
import { ChatStoreProvider } from '../state/ChatStoreProvider'

// ArtifactPanel (opened from DagNode's ⋮ menu) reads chatStore for live
// SSE follow (#1114) - every render needs the provider.
function render(ui: ReactElement) {
  return rtlRender(<ChatStoreProvider>{ui}</ChatStoreProvider>)
}

afterEach(cleanup)

const node: DagNodeDef = { id: 'r1', agent: 'web-researcher', task: 'Research Dublin.', depends_on: [] }

// Opens the panel from the ⋮ menu (the only way it opens - #1114).
async function openArtifacts(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('button', { name: 'Node actions' }))
  await user.click(await screen.findByRole('menuitem', { name: /Artifacts/ }))
  await screen.findByRole('dialog', { name: /Artifacts for node/ })
}

beforeEach(() => {
  // See ArtifactPanel.rtl.test.tsx for why these stubs are needed: jsdom has
  // no <dialog> support and no matchMedia at all, and the generated client's
  // Request construction needs an absolute base to resolve against in a
  // jsdom document.
  HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) { this.setAttribute('open', '') }
  HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) { this.removeAttribute('open') }
  vi.stubGlobal('matchMedia', vi.fn((query: string) => ({
    matches: false, media: query, addEventListener: () => {}, removeEventListener: () => {},
  })))
  client.setConfig({ baseUrl: 'http://localhost' })
  vi.stubGlobal('fetch', vi.fn(async () =>
    new Response(JSON.stringify({ data: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } }),
  ))
})

// #1114 owner feedback: "i dont like buttons with full text it should be
// moved to ... menu" - the header-level "artifacts" text button is gone;
// Artifacts is now an item in the node's existing ⋮ overflow menu.
describe('DagNode artifacts menu item (#1114)', () => {
  it('opens the artifact panel dialog from the ⋮ menu on a running node', async () => {
    const user = userEvent.setup()
    render(<DagNode node={node} state={{ status: 'running' }} runs={[]} answer="" isFinal={false} chatId="chat-1" />)

    expect(screen.queryByText('artifacts')).toBeNull() // the old text button is gone

    await user.click(screen.getByRole('button', { name: 'Node actions' }))
    const item = await screen.findByRole('menuitem', { name: /Artifacts/ })
    await user.click(item)

    expect(await screen.findByRole('dialog', { name: /Artifacts for node r1/ })).toBeTruthy()
  })

  it('still offers Artifacts on a finished (terminal) node - outputs matter most after a node completes', async () => {
    const user = userEvent.setup()
    render(<DagNode node={node} state={{ status: 'done' }} runs={[]} answer="the answer" isFinal={false} chatId="chat-1" />)

    await user.click(screen.getByRole('button', { name: 'Node actions' }))
    expect(await screen.findByRole('menuitem', { name: /Artifacts/ })).toBeTruthy()
  })

  it('renders no ⋮ menu at all on a terminal node with no chatId (nothing to show)', () => {
    render(<DagNode node={node} state={{ status: 'done' }} runs={[]} answer="the answer" isFinal={false} />)
    expect(screen.queryByRole('button', { name: 'Node actions' })).toBeNull()
  })
})

// #1178/#1216: the panel reads four narrow fields from the node - its agent
// label (nodeAgent, the panel heading), its raw task (nodeTask, Details
// only), its error (nodeError, only on a failed status) and its declared
// output kind (nodeArtifactKind). These tests drive the panel through its
// only entry point to prove each field actually crosses the component
// boundary into the result view.
describe('DagNode artifact panel props (#1178)', () => {
  it('shows the node\u2019s agent label as the panel heading, not the raw task or an artifact id', async () => {
    const user = userEvent.setup()
    render(<DagNode node={node} state={{ status: 'done' }} runs={[]} answer="the answer" isFinal={false} chatId="chat-1" />)
    await openArtifacts(user)
    expect(await screen.findByRole('heading', { level: 2, name: 'Web researcher' })).toBeTruthy()
    expect(screen.getByText("This node hasn't produced anything yet.")).toBeTruthy()
  })

  it('passes the declared output kind: the panel opens on the matching artifact, not the newest', async () => {
    const user = userEvent.setup()
    // document:spec is NEWER (latest revision 5), but the node declares
    // `text` - the plan is the result view must open on.
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const url = decodeURIComponent(input instanceof Request ? input.url : String(input))
      if (url.includes('/artifacts/text:plan/revisions')) {
        return new Response(JSON.stringify({ data: [{ revision: 2, mime_type: 'text/markdown', size: 8, kind: 'text', class: 'blob', lineage: { node_id: 'p1' } }] }), { status: 200, headers: { 'Content-Type': 'application/json' } })
      }
      if (url.includes('/artifacts/text:plan?revision=2')) {
        return new Response('# The plan\n\nPlan body line.\n', { status: 200, headers: { 'Content-Type': 'text/plain' } })
      }
      if (url.endsWith('/artifacts')) {
        return new Response(JSON.stringify({
          data: [
            { name: 'text:plan', kind: 'text', class: 'blob', latest_revision: 2, lineage: { node_id: 'p1', author: 'worker' }, revisions: [] },
            { name: 'document:spec', kind: 'document', class: 'structured', latest_revision: 5, lineage: { node_id: 'p1', author: 'worker' }, revisions: [] },
          ],
        }), { status: 200, headers: { 'Content-Type': 'application/json' } })
      }
      return new Response(JSON.stringify({ error: 'not found' }), { status: 404, headers: { 'Content-Type': 'application/json' } })
    }))
    render(<DagNode node={{ ...node, id: 'p1', artifact: 'text' }} state={{ status: 'done' }} runs={[]} answer="" isFinal={false} chatId="chat-1" />)
    await openArtifacts(user)
    // The declared-kind artifact rendered, not the newer document.
    expect(await screen.findByRole('heading', { level: 1, name: 'The plan' })).toBeTruthy()
  })

  it('passes the node error on a failed node: the failure empty state shows it', async () => {
    const user = userEvent.setup()
    render(<DagNode node={node} state={{ status: 'failed', error: 'judge gave up after 3 rounds' }} runs={[]} answer="" isFinal={false} chatId="chat-1" />)
    await openArtifacts(user)
    expect(await screen.findByText('This node failed before writing its result.')).toBeTruthy()
    // The error text appears twice: the node card's own red banner and
    // the panel's failure empty state.
    expect(screen.getAllByText('judge gave up after 3 rounds').length).toBe(2)
  })

  it('does not read a transient error on a non-failed node as a failure', async () => {
    const user = userEvent.setup()
    // markNodeError annotates rejected control actions on any status -
    // the panel must not claim the node "failed" for one.
    render(<DagNode node={node} state={{ status: 'running', error: 'stop rejected (HTTP 409)' }} runs={[]} answer="" isFinal={false} chatId="chat-1" />)
    await openArtifacts(user)
    expect(await screen.findByText("This node hasn't produced anything yet.")).toBeTruthy()
    expect(screen.queryByText('This node failed before writing its result.')).toBeNull()
  })
})
