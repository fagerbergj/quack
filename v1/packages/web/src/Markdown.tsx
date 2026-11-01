import { createLowlight } from 'lowlight';
import { visit } from 'unist-util-visit';
import type { Element, Root, RootContent, Text } from 'hast';
import bash from 'highlight.js/lib/languages/bash';
import diff from 'highlight.js/lib/languages/diff';
import go from 'highlight.js/lib/languages/go';
import javascript from 'highlight.js/lib/languages/javascript';
import json from 'highlight.js/lib/languages/json';
import markdown from 'highlight.js/lib/languages/markdown';
import python from 'highlight.js/lib/languages/python';
import sql from 'highlight.js/lib/languages/sql';
import typescript from 'highlight.js/lib/languages/typescript';
import xml from 'highlight.js/lib/languages/xml';
import yaml from 'highlight.js/lib/languages/yaml';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';

// Only the languages agents emit are registered; lowlight's common set is several times larger.
const lowlight = createLowlight({ bash, diff, go, javascript, json, markdown, python, sql, typescript, xml, yaml });

const codeText = (node: Element): string => node.children.map((c) => (c.type === 'text' ? (c as Text).value : codeText(c as Element))).join('');

function rehypeHighlight() {
  return (tree: Root) => {
    visit(tree, 'element', (node: Element, _i, parent) => {
      if (node.tagName !== 'code' || parent?.type !== 'element' || parent.tagName !== 'pre') return;
      const classes = Array.isArray(node.properties.className) ? node.properties.className.map(String) : [];
      const lang = classes.find((c) => c.startsWith('language-'))?.slice('language-'.length);
      if (!lang || !lowlight.registered(lang)) return;
      node.properties.className = ['hljs', ...classes];
      const result = lowlight.highlight(lang, codeText(node));
      if (result.children.length) node.children = result.children as Element['children'];
    });
  };
}

const CITE = /\[(s\d+)\]/gi;

// "[S7]" source references become small chips.
function rehypeCitations() {
  return (tree: Root) => {
    visit(tree, 'text', (node: Text, index, parent) => {
      if (index === undefined || !parent || (parent as Element).tagName === 'code' || !CITE.test(node.value)) return;
      CITE.lastIndex = 0;
      const out: RootContent[] = [];
      let at = 0;
      for (const m of node.value.matchAll(CITE)) {
        if (m.index > at) out.push({ type: 'text', value: node.value.slice(at, m.index) });
        out.push({ type: 'element', tagName: 'span', properties: { className: ['cite'] }, children: [{ type: 'text', value: m[1]!.toUpperCase() }] });
        at = m.index + m[0].length;
      }
      if (at < node.value.length) out.push({ type: 'text', value: node.value.slice(at) });
      parent.children.splice(index, 1, ...out);
      return index + out.length;
    });
  };
}

const components = {
  a: (p: React.ComponentProps<'a'>) => <a {...p} target="_blank" rel="noopener noreferrer" />,
  table: (p: React.ComponentProps<'table'>) => (
    <div className="max-w-full overflow-x-auto"><table {...p} /></div>
  ),
};

export function Markdown({ text }: { text: string }) {
  return (
    <div className="md min-w-0">
      <ReactMarkdown remarkPlugins={[remarkGfm]} rehypePlugins={[rehypeHighlight, rehypeCitations]} components={components}>{text}</ReactMarkdown>
    </div>
  );
}
