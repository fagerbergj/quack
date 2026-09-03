// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { DagNode } from './DagNode'
import type { DagNodeDef } from '../state/agentStream'
import { client } from '../generated/client.gen'

afterEach(cleanup)

const node: DagNodeDef = { id: 'r1', agent: 'web-researcher', task: 'Research Dublin.', depends_on: [] }

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
