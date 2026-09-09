// @vitest-environment jsdom
//
// Proves the #1284 fix: memo(TriggerMessage) + a stable `attachments` element
// collapse an unchanged live turn's re-render cost from tens-to-hundreds of ms
// (react-markdown re-parsing the whole envelope) to under a few ms.
import { describe, it, expect, afterEach } from 'vitest'
import { cleanup, render, act } from '@testing-library/react'
import { Profiler, memo, useState, useMemo } from 'react'
import { TriggerMessage } from './components/TriggerEnvelope'
import { AttachmentPreviews } from './components/AttachmentUI'

afterEach(cleanup)

// Synthetic envelopes shaped like a real GitHub-trigger PR envelope (see
// .quack/trigger-prompts-v2.md), sized to match the audit's prod captures
// (quack-1264 ~9.8kB/1 comment, dotagents-20 ~26.5kB) without committing
// real PR/comment text to the repo.
function buildEnvelope(descriptionKB: number, commentCount: number): string {
  const description = 'Lorem ipsum dolor sit amet, consectetur adipiscing elit. '.repeat(
    Math.ceil((descriptionKB * 1024) / 57),
  )
  const comments = Array.from({ length: commentCount }, (_, i) => ({
    id: `c${i}`,
    createdAt: '2026-01-01T00:00:00Z',
    author: 'reviewer',
    body: `Comment ${i}: please address this finding in the diff.`.repeat(20),
  }))
  return [
    '<permissions>review, comment</permissions>',
    '<deliverable>a review with inline comments and a verdict</deliverable>',
    '<pull_request number="1264">',
    '  <title>feat: something</title>',
    `  <description>${description}</description>`,
    '</pull_request>',
    `<comments count="${commentCount}">${JSON.stringify(comments)}</comments>`,
  ].join('\n')
}

const ENVELOPES = {
  'small (~9.8kB, 1 comment)': buildEnvelope(9, 1),
  'large (~26.5kB, 1 comment)': buildEnvelope(25, 1),
}

const N = 200
const EMPTY_PRIOR: string[] = []

// Counts + Profiler durations for N forced re-renders of a parent holding
// TriggerMessage.
function measure(envelope: string, freshAttachments: boolean) {
  let triggerRenders = 0
  const durations: number[] = []

  // memo'd wrapper so ITS OWN render count tracks whether TriggerMessage's
  // memo actually bails - an unmemoized wrapper would render N+1 times
  // regardless, masking the thing under test.
  const CountedTrigger = memo((props: Parameters<typeof TriggerMessage>[0]) => {
    triggerRenders++
    return <TriggerMessage {...props} />
  })

  function Parent() {
    const [n, setN] = useState(0)
    const stableAttachments = useMemo(() => <AttachmentPreviews previews={[]} />, [])
    ;(Parent as unknown as { bump?: () => void }).bump = () => setN(v => v + 1)
    return (
      <Profiler id="trigger" onRender={(_id, _phase, actualDuration) => durations.push(actualDuration)}>
        <CountedTrigger
          content={envelope}
          priorContents={EMPTY_PRIOR}
          chatId="c"
          attachments={freshAttachments ? <AttachmentPreviews previews={[]} /> : stableAttachments}
        />
        <span data-n={n} />
      </Profiler>
    )
  }

  render(<Parent />)
  for (let i = 0; i < N; i++) {
    act(() => {
      ;(Parent as unknown as { bump?: () => void }).bump?.()
    })
  }
  return { triggerRenders, durations }
}

function meanOf(durations: number[]): number {
  return durations.reduce((a, b) => a + b, 0) / durations.length
}

describe('TriggerMessage re-render cost (#1284)', () => {
  for (const [label, envelope] of Object.entries(ENVELOPES)) {
    it(`memo + stable attachments collapses re-renders to 1: ${label}`, () => {
      const r = measure(envelope, false)
      expect(r.triggerRenders).toBe(1)
    }, 60000)

    it(`fresh attachments still defeats memo (Chat.tsx must pass a stable element): ${label}`, () => {
      const r = measure(envelope, true)
      expect(r.triggerRenders).toBe(N + 1)
    }, 60000)

    it(`stays under a few ms per re-render once attachments is stable: ${label}`, () => {
      const r = measure(envelope, false)
      const mean = meanOf(r.durations)
      // Only 1 real render happens; remaining samples are cheap bail-outs.
      // The old unmemoized behavior measured 45-123ms per re-render.
      expect(mean).toBeLessThan(5)
    }, 60000)
  }
})
