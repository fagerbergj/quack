// @vitest-environment jsdom
// #1284/#1300: memo(TriggerMessage) must collapse a live turn's re-renders to
// one body call while props (crucially `attachments`) stay referentially stable - pinned via triggerMessageRenderProbe, not duration (jsdom timings too noisy).
import { describe, it, expect, afterEach, beforeEach } from 'vitest'
import { cleanup, render, act, screen } from '@testing-library/react'
import { useState, useMemo } from 'react'
import { TriggerMessage, triggerMessageRenderProbe } from './components/TriggerEnvelope'
import { AttachmentPreviews } from './components/AttachmentUI'

afterEach(cleanup)
beforeEach(() => { triggerMessageRenderProbe.count = 0 })

// Shaped like a real GitHub-trigger PR envelope (.quack/trigger-prompts-v2.md)
// without committing real PR text; content size is irrelevant to a render-count pin, so one small fixture.
const ENVELOPE = [
  '<permissions>review, comment</permissions>',
  '<deliverable>a review with inline comments and a verdict</deliverable>',
  '<pull_request number="1264">',
  '  <title>feat: something</title>',
  '  <description>Lorem ipsum dolor sit amet, consectetur adipiscing elit.</description>',
  '</pull_request>',
  '<comments count="1">[{"id":"c0","createdAt":"2026-01-01T00:00:00Z","author":"reviewer","body":"please address this finding in the diff"}]</comments>',
].join('\n')

const N = 200
const EMPTY_PRIOR: string[] = []

function Parent({ freshAttachments }: { freshAttachments: boolean }) {
  const [n, setN] = useState(0)
  const stableAttachments = useMemo(() => <AttachmentPreviews previews={[]} />, [])
  ;(Parent as unknown as { bump?: () => void }).bump = () => setN(v => v + 1)
  return (
    <>
      <TriggerMessage
        content={ENVELOPE}
        priorContents={EMPTY_PRIOR}
        chatId="c"
        attachments={freshAttachments ? <AttachmentPreviews previews={[]} /> : stableAttachments}
      />
      <span data-n={n} />
    </>
  )
}

function bumpNTimes() {
  for (let i = 0; i < N; i++) {
    act(() => {
      ;(Parent as unknown as { bump?: () => void }).bump?.()
    })
  }
}

describe('TriggerMessage re-render cost (#1284)', () => {
  it('memo(TriggerMessage) collapses N unrelated parent re-renders to 1 when props are stable', () => {
    render(<Parent freshAttachments={false} />)
    bumpNTimes()
    // Fails if memo(TriggerMessage) is removed: every one of the N re-renders
    // would then reach the production function body instead of bailing.
    expect(triggerMessageRenderProbe.count).toBe(1)
  }, 60000)

  it('fresh attachments still defeats memo (Chat.tsx must pass a stable element)', () => {
    render(<Parent freshAttachments={true} />)
    bumpNTimes()
    expect(triggerMessageRenderProbe.count).toBe(N + 1)
  }, 60000)
})

// #1300 review: memo(TriggerMessage) does a shallow prop compare, so only
// reference equality (via Chat.tsx's useMemo) may suppress a re-render, not
// the new element's shape/length. Same-length, different-content swaps are what a naive comparator gets wrong.
describe('TriggerMessage attachments prop (#1300 review)', () => {
  it('re-renders and shows new attachments when the prop changes, even at the same length', () => {
    const { rerender } = render(
      <TriggerMessage content="plain message" attachments={<AttachmentPreviews previews={[{ url: 'a.png', mime: 'image/png', name: 'first' }]} />} />,
    )
    expect(screen.getAllByAltText('first').length).toBeGreaterThan(0)

    rerender(
      <TriggerMessage content="plain message" attachments={<AttachmentPreviews previews={[{ url: 'b.png', mime: 'image/png', name: 'second' }]} />} />,
    )
    expect(screen.queryAllByAltText('first')).toHaveLength(0)
    expect(screen.getAllByAltText('second').length).toBeGreaterThan(0)
  })
})
