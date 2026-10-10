// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { cleanup, render, type RenderResult } from '@testing-library/react'
import { AssistantText } from './AgentParts'

afterEach(cleanup)

// Spies on react-markdown to see which chunks AssistantText hands it: the probe for "did the settled
// prefix render again".
const { calls } = vi.hoisted(() => ({ calls: [] as string[] }))
vi.mock('react-markdown', async importOriginal => {
  const actual = await importOriginal<typeof import('react-markdown')>()
  function Spy(props: Record<string, unknown>) {
    calls.push(String(props.children))
    return actual.default(props as never)
  }
  return { ...actual, default: Spy }
})

const PARA = 'Some prose about the answer, several words long to pad the block out further.'
function block(i: number): string {
  return `## Section ${i}\n\n${PARA} ${PARA} ${PARA}\n\n`
}

describe('AssistantText streaming split (audit finding 2)', () => {
  let view: RenderResult | undefined

  afterEach(() => {
    calls.length = 0
  })

  function mount(text: string, streaming: boolean) {
    view = render(<AssistantText text={text} streaming={streaming} />)
  }

  function update(text: string, streaming: boolean) {
    view!.rerender(<AssistantText text={text} streaming={streaming} />)
  }

  it('streams ~20k chars in small chunks without reprocessing the whole document per chunk', () => {
    // Small (~60-char) chunks like real tokens, the shape that made the unsplit render O(n^2).
    // ~340 real renders, hence the raised timeout.
    let full = ''
    let i = 0
    while (full.length < 20000) full += block(i++)
    const CHUNK = 60
    let text = ''
    mount(text, true)
    while (text.length < full.length) {
      text = full.slice(0, text.length + CHUNK)
      update(text, true)
    }
    const updateCount = Math.ceil(full.length / CHUNK)

    // Every prefix-sized call is a frozen prefix and must appear exactly once across the stream.
    const counts = new Map<string, number>()
    for (const c of calls) counts.set(c, (counts.get(c) ?? 0) + 1)
    let prefixCallsSeen = 0
    for (const [content, count] of counts) {
      if (content.length > 3000) {
        expect(count).toBe(1)
        prefixCallsSeen++
      }
    }
    expect(prefixCallsSeen).toBeGreaterThan(0) // the split actually engaged during this stream

    // Unsplit, total parsed chars grow ~CHUNK * updates^2 / 2; the frozen prefix must beat that even with
    // dense blank lines forcing extra re-freezes.
    const totalCharsProcessed = calls.reduce((sum, c) => sum + c.length, 0)
    const unsplitWouldProcess = CHUNK * updateCount * (updateCount + 1) / 2
    expect(totalCharsProcessed).toBeLessThan(unsplitWouldProcess / 2)
  }, 60000)

  it('never splits inside an open fence even when it spans past the live-tail window', () => {
    const prose = block(0) + block(1) + block(2)
    // Blank lines inside the fence: a naive "\n\n" search would wrongly split here without the fence guard.
    const fenceBody = Array.from({ length: 400 }, (_, i) => `func f${i}() {}`).join('\n\n')
    const text = prose + '```go\n' + fenceBody // deliberately never closed - still streaming
    mount(text, true)

    // The open fence's start and latest content must land in the same (live-tail) call, or rehype sees
    // an unterminated fence twice.
    const fenceCall = calls.find(c => c.includes('```go'))
    expect(fenceCall).toBeDefined()
    expect(fenceCall).toContain('func f399() {}')
  })

  it('renders identical HTML once streaming ends vs. a fresh single-shot render of the same text', () => {
    let text = ''
    let i = 0
    mount(text, true)
    while (text.length < 8000) {
      text += block(i++)
      update(text, true)
    }
    // The reference definition arrives last, so a still-split final render would leave the link unresolved.
    text += 'See [the docs][ref] again here.\n\n| a | b |\n| - | - |\n| 1 | 2 |\n\n[ref]: https://example.com/docs\n'
    update(text, false) // stream complete: single-document render
    const splitEndHtml = view!.container.innerHTML

    const text2 = text
    cleanup()
    calls.length = 0
    mount(text2, false)
    const singleShotHtml = view!.container.innerHTML

    expect(splitEndHtml).toBe(singleShotHtml)
    expect(splitEndHtml).toContain('href="https://example.com/docs"')
  })
})
