// @vitest-environment jsdom
// Adversarial split points: post-stream output must match a fresh render; mid-stream must not lose content.
import { describe, it, expect, afterEach } from 'vitest'
import { cleanup, render } from '@testing-library/react'
import { AssistantText } from './AgentParts'
import { lastSafeSplitOffset } from './mermaidSource'

afterEach(cleanup)

function renderHtml(text: string, streaming = false): string {
  const { container, unmount } = render(<AssistantText text={text} streaming={streaming} />)
  const html = container.innerHTML
  unmount()
  return html
}

// Simulates the frozen/live split directly, rejoining both halves in one div like production.
function splitHtml(text: string, cut: number): string {
  const unwrap = (html: string) => html.replace(/^<div class="prose[^"]*">/, '').replace(/<\/div>$/, '')
  return `<div class="prose prose-sm dark:prose-invert max-w-none break-words">`
    + unwrap(renderHtml(text.slice(0, cut))) + unwrap(renderHtml(text.slice(cut))) + `</div>`
}

// react-markdown emits a newline between sibling blocks that a join of two parses lacks; it's invisible,
// so ignore it.
function normTags(html: string): string {
  return html.replace(/>\s+</g, '><')
}

// Streams growing text, then commits streaming=false the way Chat.tsx does when a turn completes.
function streamThenFinish(full: string, chunk = 40): string {
  let text = ''
  const { container, rerender, unmount } = render(<AssistantText text={text} streaming />)
  while (text.length < full.length) {
    text = full.slice(0, text.length + chunk)
    rerender(<AssistantText text={text} streaming />)
  }
  rerender(<AssistantText text={full} streaming={false} />)
  const html = container.innerHTML
  unmount()
  return html
}

describe('AssistantText split boundary safety', () => {
  it('a footnote multi-paragraph definition never splits mid-definition, and final render matches non-streaming', () => {
    const text = 'See note.[^1]\n\n[^1]: First paragraph of note.\n\n    Second paragraph of note.\n\nMore text.\n'
    const innerBlank = text.indexOf('First paragraph of note.') + 'First paragraph of note.'.length
    const idx = lastSafeSplitOffset(text, innerBlank + 2)
    // Must not land inside the footnote definition - either falls back
    // before it starts, or refuses to split at all.
    expect(idx).toBeLessThanOrEqual(text.indexOf('[^1]:'))

    expect(() => streamThenFinish(text)).not.toThrow()
    expect(streamThenFinish(text)).toBe(renderHtml(text, false))
  })

  it('an indented code block with an internal blank line never splits mid-block, and final render matches non-streaming', () => {
    const text = 'Intro.\n\n    line one indented\n\n    line two indented\n\nOutro.\n'
    const innerBlank = text.indexOf('line one indented') + 'line one indented'.length
    const idx = lastSafeSplitOffset(text, innerBlank + 2)
    expect(idx).toBeLessThanOrEqual(text.indexOf('line one indented'))

    expect(() => streamThenFinish(text)).not.toThrow()
    expect(streamThenFinish(text)).toBe(renderHtml(text, false))
    // The final, non-split render keeps it as ONE code block (the blank
    // line is content, not a boundary).
    expect(renderHtml(text, false).match(/<pre>/g)?.length).toBe(1)
  })

  // A loose list's item boundary isn't caught by the lazy-continuation guard and yields two <ul>s;
  // accepted because it self-heals when streaming ends.
  it('a loose list split at an item boundary is a transient visual artifact that heals once streaming ends', () => {
    const text = 'Intro.\n\n- item one\n\n  continuation paragraph within same item\n\n- item two\n\nOutro.\n'
    const itemBoundary = text.indexOf('same item') + 'same item'.length
    const idx = lastSafeSplitOffset(text, itemBoundary + 2)
    expect(idx).toBeGreaterThan(0) // does split here - the known limitation
    expect(() => splitHtml(text, idx)).not.toThrow()

    expect(() => streamThenFinish(text)).not.toThrow()
    expect(streamThenFinish(text)).toBe(renderHtml(text, false))
    expect(renderHtml(text, false).match(/<ul>/g)?.length).toBe(1) // one list, final
  })

  it('a setext heading can never be split between its text and underline (no blank line exists there)', () => {
    const text = 'Intro para.\n\nFoo\n===\n\nBar text.\n'
    const idx = lastSafeSplitOffset(text, text.length)
    expect(idx).toBeGreaterThan(0)
    // Whatever boundary is chosen, "Foo" and "===" must stay on the same side.
    const headingSide = text.slice(0, idx).includes('Foo') ? text.slice(0, idx) : text.slice(idx)
    expect(headingSide).toContain('Foo\n===')
    expect(normTags(splitHtml(text, idx))).toBe(normTags(renderHtml(text, false)))
    expect(streamThenFinish(text)).toBe(renderHtml(text, false))
  })

  it('a GFM table never splits mid-table (rows cannot contain a blank line)', () => {
    const text = 'Intro.\n\n| a | b |\n| - | - |\n| 1 | 2 |\n| 3 | 4 |\n\nOutro.\n'
    const idx = lastSafeSplitOffset(text, text.length)
    expect(idx).toBeGreaterThan(0)
    expect(normTags(splitHtml(text, idx))).toBe(normTags(renderHtml(text, false)))
    expect(renderHtml(text, false)).toContain('<table>')
  })

  it('a multi-line link reference definition resolves the same split or not', () => {
    const text = 'See [the docs][ref] here.\n\n[ref]: https://example.com/docs\n  "A Title\n  spanning two lines"\n\nOutro.\n'
    const idx = lastSafeSplitOffset(text, text.length)
    const whole = renderHtml(text, false)
    expect(whole).toContain('href="https://example.com/docs"')
    if (idx > 0) expect(normTags(splitHtml(text, idx))).toBe(normTags(whole))
    expect(streamThenFinish(text)).toBe(whole)
  })

  it('an HTML block (<details>, used by the researcher) never splits mid-block when its content has no blank line', () => {
    const text = 'Intro.\n\n<details>\n<summary>reasoning</summary>\nSome reasoning text here.\n</details>\n\nOutro.\n'
    const idx = lastSafeSplitOffset(text, text.length)
    const whole = renderHtml(text, false)
    expect(whole).toContain('<details>')
    expect(whole).toContain('</details>')
    if (idx > 0) expect(normTags(splitHtml(text, idx))).toBe(normTags(whole))
    expect(streamThenFinish(text)).toBe(whole)
  })

  it('a fenced code block inside a blockquote is invisible to the fence walker but has no real blank line to split on', () => {
    // A blank line always ends a blockquote in CommonMark, so there's no unsafe internal boundary here.
    const text = 'Intro.\n\n> ```js\n> const a = 1\n>\n> const b = 2\n> ```\n\nOutro.\n'
    const idx = lastSafeSplitOffset(text, text.length)
    const whole = renderHtml(text, false)
    expect(whole).toContain('<blockquote>')
    if (idx > 0) expect(normTags(splitHtml(text, idx))).toBe(normTags(whole))
    expect(streamThenFinish(text)).toBe(whole)
  })

  it('a fence nested inside a differently-fenced block never splits on the inner blank line', () => {
    const text = 'Intro.\n\n~~~text\nExample:\n\n```js\ncode here\n```\n~~~\n\nOutro.\n'
    const innerBlank = text.indexOf('Example:') + 'Example:'.length
    const idx = lastSafeSplitOffset(text, innerBlank + 2)
    expect(idx).toBeLessThanOrEqual(text.indexOf('~~~text'))
    expect(streamThenFinish(text)).toBe(renderHtml(text, false))
  })
})
