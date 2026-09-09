import { describe, it, expect } from 'vitest'
import rehypeHighlightSubset from './rehypeHighlightSubset'
import type { Root, Element, Text } from 'hast'

function tree(lang: string, code: string): Root {
  const codeNode: Element = { type: 'element', tagName: 'code', properties: { className: lang ? [`language-${lang}`] : [] }, children: [{ type: 'text', value: code }] }
  const preNode: Element = { type: 'element', tagName: 'pre', properties: {}, children: [codeNode] }
  return { type: 'root', children: [preNode] }
}

function codeNodeOf(t: Root): Element {
  return ((t.children[0] as Element).children[0]) as Element
}

describe('rehypeHighlightSubset', () => {
  it('highlights a language in the subset into hljs spans', () => {
    const t = tree('go', 'func main() {}')
    rehypeHighlightSubset()(t)
    const code = codeNodeOf(t)
    expect(code.properties.className).toContain('hljs')
    expect(code.children.some(c => c.type === 'element')).toBe(true)
  })

  it('leaves a language outside the subset untouched, no throw', () => {
    const t = tree('ruby', 'def x; end')
    rehypeHighlightSubset()(t)
    const code = codeNodeOf(t)
    expect(code.properties.className).toEqual(['language-ruby'])
    expect(code.children).toEqual([{ type: 'text', value: 'def x; end' }])
  })

  it('ignores a code block with no language class', () => {
    const t = tree('', 'plain text')
    rehypeHighlightSubset()(t)
    const code = codeNodeOf(t)
    expect((code.children[0] as Text).value).toBe('plain text')
  })
})
