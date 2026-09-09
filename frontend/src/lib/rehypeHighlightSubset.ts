import { createLowlight } from 'lowlight'
import { visit } from 'unist-util-visit'
import type { Element, Root, Text } from 'hast'
import go from 'highlight.js/lib/languages/go'
import typescript from 'highlight.js/lib/languages/typescript'
import javascript from 'highlight.js/lib/languages/javascript'
import python from 'highlight.js/lib/languages/python'
import bash from 'highlight.js/lib/languages/bash'
import json from 'highlight.js/lib/languages/json'
import yaml from 'highlight.js/lib/languages/yaml'
import xml from 'highlight.js/lib/languages/xml'
import markdown from 'highlight.js/lib/languages/markdown'
import sql from 'highlight.js/lib/languages/sql'
import diff from 'highlight.js/lib/languages/diff'

// rehype-highlight statically imports lowlight's `common` set (37 grammars,
// 156 kB gzip) with no way to shrink it via options (#1297). Agents only ever
// emit these 11 languages, so registering just them is the whole fix.
const lowlight = createLowlight({ go, typescript, javascript, python, bash, json, yaml, xml, markdown, sql, diff })

// codeText concatenates a hast node's text descendants - same shape as
// AgentParts.tsx's hastText, pulled local rather than adding hast-util-to-text
// as another dependency for one string-join.
function codeText(node: Element): string {
  return node.children.map(c => (c.type === 'text' ? (c as Text).value : codeText(c as Element))).join('')
}

// Minimal reimplementation of rehype-highlight's `<pre><code class="language-x">`
// walk - detect-on-no-class and aliasing are dropped since nothing here used them.
export default function rehypeHighlightSubset() {
  return (tree: Root) => {
    visit(tree, 'element', (node: Element, _index, parent) => {
      if (node.tagName !== 'code' || parent?.type !== 'element' || parent.tagName !== 'pre') return
      const classes = Array.isArray(node.properties.className) ? node.properties.className : []
      const lang = classes.map(String).find(c => c.startsWith('language-'))?.slice('language-'.length)
      if (!lang || !lowlight.registered(lang)) return
      if (!classes.includes('hljs')) classes.unshift('hljs')
      node.properties.className = classes
      const result = lowlight.highlight(lang, codeText(node))
      if (result.children.length > 0) node.children = result.children as Element['children']
    })
  }
}
