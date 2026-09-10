// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { AssistantText } from './AgentParts'

// Spies on react-markdown's default export so a test can see exactly which
// text chunks AssistantText hands it, without touching production code -
// the render-count probe for "did the settled prefix render again".
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
  let root: ReturnType<typeof createRoot> | undefined
  let host: HTMLDivElement | undefined

  afterEach(() => {
    act(() => root?.unmount())
    host?.remove()
    root = undefined
    host = undefined
    calls.length = 0
  })

  function mount(text: string, streaming: boolean) {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
    // @ts-expect-error react act environment flag
    globalThis.IS_REACT_ACT_ENVIRONMENT = true
    act(() => { root!.render(createElement(AssistantText, { text, streaming })) })
  }

  function update(text: string, streaming: boolean) {
    act(() => { root!.render(createElement(AssistantText, { text, streaming })) })
  }

  it('streams ~20k chars in small chunks without reprocessing the whole document per chunk', () => {
    // Reveal the full text in small (~60-char) chunks like real tokens
    // (audit: ~100 ms apart at 10 tok/s, each with its own commit) - the
    // shape that made the unsplit render O(n^2). ~340 real renders, hence the raised timeout.
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

    // Every prefix-sized call (well past the ~2000-char live tail window) is
    // a settled prefix for the memoized FrozenAssistantDocument - each must
    // appear exactly once across the stream, proving a frozen prefix is never handed back in later.
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

    // Regression guard: total characters ReactMarkdown ever parsed. Unsplit
    // this is ~CHUNK * updateCount^2 / 2 (quadratic in answer size, per the
    // audit); the frozen prefix beats it even though dense blank lines force more re-freezes than the audit's benchmark saw.
    const totalCharsProcessed = calls.reduce((sum, c) => sum + c.length, 0)
    const unsplitWouldProcess = CHUNK * updateCount * (updateCount + 1) / 2
    expect(totalCharsProcessed).toBeLessThan(unsplitWouldProcess / 2)
  }, 15000)

  it('never splits inside an open fence even when it spans past the live-tail window', () => {
    const prose = block(0) + block(1) + block(2)
    // Blank lines between "functions" inside the fence - the exact shape a
    // naive backward search for the nearest "\n\n" would wrongly treat as a
    // safe split point if the fence guard were missing.
    const fenceBody = Array.from({ length: 400 }, (_, i) => `func f${i}() {}`).join('\n\n')
    const text = prose + '```go\n' + fenceBody // deliberately never closed - still streaming
    mount(text, true)

    // The open fence's start and its latest content must land in the SAME
    // ReactMarkdown call (the live tail's) - never split across a frozen
    // prefix and a tail, which would hand rehype an unterminated fence twice.
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
    // A link whose reference definition arrives only at the very end - if the
    // final render were still split, the frozen prefix (parsed before the
    // definition existed) would never resolve it.
    text += 'See [the docs][ref] again here.\n\n| a | b |\n| - | - |\n| 1 | 2 |\n\n[ref]: https://example.com/docs\n'
    update(text, false) // stream complete: single-document render
    const splitEndHtml = host!.innerHTML

    const text2 = text
    root!.unmount()
    host!.remove()
    calls.length = 0
    mount(text2, false)
    const singleShotHtml = host!.innerHTML

    expect(splitEndHtml).toBe(singleShotHtml)
    expect(splitEndHtml).toContain('href="https://example.com/docs"')
  })
})
